// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	sessionDirectoryMode = 0o700
	sessionFileMode      = 0o600
)

var errSessionState = errors.New("invalid session state")

// SessionStatus contains no child output or authentication data.
type SessionStatus struct {
	Time          time.Time   `json:"time"`
	Alias         string      `json:"alias"`
	Vendor        string      `json:"vendor"`
	Phase         string      `json:"phase"`
	Reason        string      `json:"reason"`
	Restarts      []time.Time `json:"restarts"`
	LoginExpiring bool        `json:"login_expiring"` //nolint:tagliatelle // The public status field uses snake_case.
}

func sessionIdentity(vendor, account string) bool {
	return (vendor == "claude" || vendor == "codex") && ValidAlias(account)
}

// SessionRoot opens a canonical spool boundary, rejecting symbolic links.
func SessionRoot(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, errSessionState
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != filepath.Clean(dir) {
		return nil, errSessionState
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errSessionState
	}

	return root, nil
}

// Stopped checks both operator controls without opening their contents.
func Stopped(root *os.Root, account string) (bool, error) {
	if !ValidAlias(account) {
		return false, errSessionState
	}

	for _, name := range []string{"stop", account + ".stop"} {
		_, err := root.Lstat(name)
		if err == nil {
			return true, nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return false, errSessionState
		}
	}

	return false, nil
}

// SnapshotTime inspects only this alias's snapshot metadata.
func SnapshotTime(root *os.Root, vendor, account string) (time.Time, error) {
	if !sessionIdentity(vendor, account) {
		return time.Time{}, errSessionState
	}

	info, err := root.Lstat(vendor + "-" + account + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}

	if err != nil || !info.Mode().IsRegular() {
		return time.Time{}, errSessionState
	}

	return info.ModTime(), nil
}

func statusRoot(root *os.Root) (*os.Root, error) {
	err := root.Mkdir("status", sessionDirectoryMode)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, errSessionState
	}

	info, err := root.Lstat("status")
	if err != nil || !info.IsDir() {
		return nil, errSessionState
	}

	dir, err := root.OpenRoot("status")
	if err != nil {
		return nil, errSessionState
	}

	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = dir.Close()

		return nil, errSessionState
	}

	return dir, nil
}

// WriteSession replaces an alias's private status atomically.
func WriteSession(root *os.Root, status SessionStatus) error {
	if !sessionIdentity(status.Vendor, status.Alias) || status.Time.IsZero() {
		return errSessionState
	}

	dir, err := statusRoot(root)
	if err != nil {
		return errSessionState
	}
	defer dir.Close()

	name := status.Vendor + "-" + status.Alias + ".json"

	info, err := dir.Lstat(name)
	if err == nil && !info.Mode().IsRegular() {
		return errSessionState
	}

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errSessionState
	}

	data, err := json.Marshal(status)
	if err != nil {
		return errSessionState
	}

	temporary := ".status-" + rand.Text()

	file, err := dir.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sessionFileMode)
	if err != nil {
		return errSessionState
	}
	defer dir.Remove(temporary) //nolint:errcheck // Rename consumes the temporary file.

	_, writeErr := file.Write(data)
	if errors.Join(writeErr, file.Sync(), file.Close()) != nil {
		return errSessionState
	}

	if dir.Rename(temporary, name) != nil {
		return errSessionState
	}

	return nil
}

// ReadSession reads only the requested alias's status, for durable restart limits.
func ReadSession(root *os.Root, vendor, account string) (SessionStatus, error) {
	if !sessionIdentity(vendor, account) {
		return SessionStatus{}, errSessionState
	}

	dir, err := statusRoot(root)
	if err != nil {
		return SessionStatus{}, errSessionState
	}
	defer dir.Close()

	name := vendor + "-" + account + ".json"

	info, err := dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		var empty SessionStatus

		return empty, nil
	}

	if err != nil || !info.Mode().IsRegular() {
		return SessionStatus{}, errSessionState
	}

	file, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return SessionStatus{}, errSessionState
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return SessionStatus{}, errSessionState
	}

	var status SessionStatus
	if json.NewDecoder(file).Decode(&status) != nil || status.Vendor != vendor || status.Alias != account {
		return SessionStatus{}, errSessionState
	}

	return status, nil
}
