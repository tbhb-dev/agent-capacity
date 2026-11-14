// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package spool stores only captured usage subtrees.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

// ErrDirectory reports missing explicit configuration without exposing values.
var ErrDirectory = errors.New("spool directory is required")

var alias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidAlias checks a username before it becomes a filename.
func ValidAlias(account string) bool {
	return alias.MatchString(account)
}

// Snapshot stores one account's captured usage subtree.
type Snapshot struct {
	ObservedAt time.Time       `json:"observed_at"` //nolint:tagliatelle // The input format uses snake_case field names.
	Vendor     string          `json:"vendor"`
	Account    string          `json:"account"`
	Data       json.RawMessage `json:"data"`
}

// ValidIdentity rejects private identifiers and unsupported vendors.
func ValidIdentity(vendor, account string) bool {
	return (vendor == "claude" || vendor == "agy") && ValidAlias(account)
}

// Write replaces a snapshot using a private temporary file in the same directory.
func Write(dir string, snapshot Snapshot) error {
	return write(dir, snapshot, os.CreateTemp)
}

func write(dir string, snapshot Snapshot, create func(string, string) (*os.File, error)) error {
	if dir == "" {
		return ErrDirectory
	}

	if !ValidIdentity(snapshot.Vendor, snapshot.Account) || snapshot.ObservedAt.IsZero() {
		return capacity.ErrInvalidReading
	}

	snapshot.ObservedAt = snapshot.ObservedAt.UTC()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return capacity.ErrInvalidReading
	}

	err = os.MkdirAll(dir, 0o700)
	if err != nil {
		return fmt.Errorf("create spool: %w", err)
	}

	file, err := create(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())

	_, writeErr := file.Write(data)

	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return err
	}

	err = os.Rename(file.Name(), filepath.Join(dir, snapshot.Vendor+"-"+snapshot.Account+".json"))

	return errors.Join(err)
}

// Read skips individual failed files and returns their safe error classes.
func Read(dir, vendor string) ([]Snapshot, []string, error) {
	if dir == "" {
		return nil, nil, ErrDirectory
	}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("read spool: %w", err)
	}

	var snapshots []Snapshot

	var failures []string

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), vendor+"-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		snapshot, readErr := readFile(dir, entry.Name())
		if readErr != nil {
			failures = append(failures, capacity.ErrorClass(readErr))

			continue
		}

		snapshots = append(snapshots, snapshot)
	}

	return snapshots, failures, nil
}

func readFile(dir, name string) (Snapshot, error) {
	var snapshot Snapshot

	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot: %w", err)
	}

	if json.Unmarshal(data, &snapshot) != nil || !ValidIdentity(snapshot.Vendor, snapshot.Account) ||
		name != snapshot.Vendor+"-"+snapshot.Account+".json" ||
		snapshot.ObservedAt.IsZero() || len(snapshot.Data) == 0 {
		return Snapshot{}, capacity.ErrInvalidReading
	}

	return snapshot, nil
}
