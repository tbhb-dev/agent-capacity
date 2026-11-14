// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package spool

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	sessionDirectoryMode = 0o700
	sessionFileMode      = 0o600
	// maxStatusBytes bounds a status read. A full status with its restart history is under 2 KiB.
	maxStatusBytes = 8 << 10
	// maxRestartHistory is four times the restart limit of five per hour.
	maxRestartHistory = 20
	// unsafeWriteBits are the group and world write permission bits.
	unsafeWriteBits = 0o022
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
	// gomutants:disable-next-line EXPRESSION_REMOVE reason="An empty path from a failed resolution never matches."
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

// Tests replace these five calls to fail the status directory or file after a check. No input
// triggers that change inside one function. The hook lets tests trigger the eight outcomes it guards.
//
//nolint:gochecknoglobals // Tests replace the calls to simulate a concurrent filesystem change.
var (
	lstat   = (*os.Root).Lstat
	stat    = (*os.Root).Stat
	rename  = (*os.Root).Rename
	fstat   = (*os.File).Stat
	syncDir = syncRoot
)

// syncRoot flushes the directory entries of dir.
func syncRoot(dir *os.Root) error {
	handle, err := dir.Open(".")
	if err != nil {
		return errSessionState
	}

	return errors.Join(handle.Sync(), handle.Close())
}

func statusRoot(root *os.Root) (*os.Root, error) {
	err := root.Mkdir("status", sessionDirectoryMode)
	// gomutants:disable-next-line BRANCH_IF reason="A missing directory fails the next check the same way."
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, errSessionState
	}

	if err == nil && syncDir(root) != nil {
		// Removing the directory makes a retry create and sync it again.
		root.Remove("status") //nolint:errcheck,gosec // The failed sync is the error to report.

		return nil, errSessionState
	}

	info, err := lstat(root, "status")
	if err != nil || !info.IsDir() || !privateEntry(info) {
		return nil, errSessionState
	}

	dir, err := root.OpenRoot("status")
	if err != nil {
		return nil, errSessionState
	}

	opened, err := stat(dir, ".")
	// gomutants:disable-next-line EXPRESSION_REMOVE reason="A nil Stat result also fails SameFile."
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

	info, err := lstat(dir, name)
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
	// gomutants:disable-next-line BRANCH_IF reason="Writing to the nil file fails the next check the same way."
	if err != nil {
		return errSessionState
	}

	defer dir.Remove(temporary) //nolint:errcheck // Rename consumes the temporary file.

	_, writeErr := file.Write(data)
	if errors.Join(writeErr, file.Sync(), file.Close()) != nil {
		return errSessionState
	}

	if rename(dir, temporary, name) != nil || syncDir(dir) != nil {
		return errSessionState
	}

	return nil
}

// privateEntry reports whether the current user is the owner and the group and others can't write.
func privateEntry(info fs.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)

	return ok && int(owner.Uid) == os.Getuid() && info.Mode().Perm()&unsafeWriteBits == 0
}

// ReadBounded reads a regular file of at most limit bytes without following links or blocking on a pipe.
func ReadBounded(root *os.Root, name string, limit int64) ([]byte, error) {
	data, _, err := readRegular(root, name, limit)

	return data, err
}

// readRegular returns fs.ErrNotExist for a missing file and errSessionState for every other failure.
func readRegular(dir *os.Root, name string, limit int64) ([]byte, fs.FileInfo, error) {
	info, err := lstat(dir, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}

	if err != nil {
		return nil, nil, errSessionState
	}

	// gomutants:disable-next-line INTEGER_INCREMENT reason="The mode argument is ignored when no file is created."
	file, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	// gomutants:disable-next-line BRANCH_IF reason="The nil file also fails the Stat that follows."
	if err != nil {
		return nil, nil, errSessionState
	}

	defer file.Close()

	opened, err := fstat(file)
	// gomutants:disable-next-line EXPRESSION_REMOVE reason="SameFile rejects a nil Stat result."
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, nil, errSessionState
	}

	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))

	var oversize error
	if int64(len(data)) > limit {
		oversize = errSessionState
	}

	if errors.Join(readErr, oversize) != nil {
		return nil, nil, errSessionState
	}

	return data, opened, nil
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

	data, info, err := readRegular(dir, vendor+"-"+account+".json", maxStatusBytes)
	if errors.Is(err, fs.ErrNotExist) {
		var empty SessionStatus

		// gomutants:disable-next-line RETURN_ZERO reason="The empty status is already the zero value."
		return empty, nil
	}

	if err != nil || !privateEntry(info) {
		return SessionStatus{}, errSessionState
	}

	var status SessionStatus
	if strictJSON(data) != nil || json.Unmarshal(data, &status) != nil || len(status.Restarts) > maxRestartHistory ||
		status.Vendor != vendor || status.Alias != account {
		return SessionStatus{}, errSessionState
	}

	return status, nil
}

// strictJSON refuses duplicate or case-variant keys and any data after the one JSON value.
func strictJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	if checkValue(dec) != nil {
		return errSessionState
	}

	_, err := dec.Token()
	if !errors.Is(err, io.EOF) {
		return errSessionState
	}

	return nil
}

func checkValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return errSessionState
	}

	switch token {
	case json.Delim('{'):
		return checkMembers(dec)
	case json.Delim('['):
		return checkElements(dec)
	default:
		return nil
	}
}

func checkMembers(dec *json.Decoder) error {
	seen := map[string]struct{}{}

	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return errSessionState
		}

		key := strings.ToLower(fmt.Sprint(name))
		if _, duplicate := seen[key]; duplicate {
			return errSessionState
		}

		seen[key] = struct{}{}

		if checkValue(dec) != nil {
			return errSessionState
		}
	}

	_, err := dec.Token()

	return errors.Join(err)
}

func checkElements(dec *json.Decoder) error {
	for dec.More() {
		if checkValue(dec) != nil {
			return errSessionState
		}
	}

	_, err := dec.Token()

	return errors.Join(err)
}
