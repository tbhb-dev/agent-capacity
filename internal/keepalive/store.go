// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package keepalive

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// ErrStateInvalid contains no paths or input values.
var ErrStateInvalid = errors.New(stateInvalid)

const privateFileMode = 0o600

// Store holds a private spool. The owner serializes operations.
// Create stop for all aliases, or <alias>.stop for one alias.
// Callers provide an absolute path and keep the directory under exclusive ownership.
// Store writes a temporary file in the spool and syncs it before replacing a
// record. A new ledger uses a link to avoid replacing earlier attempts. The
// owner must preserve the ledger when restoring a spool, because an uncertain
// attempt still consumes the count cap. ReadStatus returns only the public
// status fields. A status reader doesn't need to decode the attempt log.
// Stop files use the same directory boundary as records.
type Store struct {
	Dir string
}

// spoolFiles lets fault fixtures exercise each durable write failure. Production
// operations always use os.Root to enforce the directory boundary.
type spoolFiles interface {
	Lstat(name string) (fs.FileInfo, error)
	Open(name string) (*os.File, error)
	OpenFile(name string, flags int, mode fs.FileMode) (*os.File, error)
	Remove(name string) error
	Link(oldName, newName string) error
	Rename(oldName, newName string) error
}

func validEnrollment(enrollment Enrollment, allow []string) bool {
	if !slices.Contains(allow, enrollment.Alias) || !validAlias(enrollment.Alias) {
		return false
	}

	switch enrollment.Vendor {
	case vendorClaude, vendorAGY, vendorCodex:
	default:
		return false
	}

	if !ratio(enrollment.DebitAllowance) || len(enrollment.Windows) == 0 {
		return false
	}

	seen := make(map[string]bool)

	for _, window := range enrollment.Windows {
		switch window {
		case "5h", "7d", "7d_opus", "7d_sonnet", "other":
		default:
			return false
		}

		if seen[window] {
			return false
		}

		seen[window] = true
	}

	return true
}

func validAlias(alias string) bool {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`).MatchString(alias)
}

func validStatus(status Status) bool {
	if !validAlias(status.Alias) || status.At.IsZero() {
		return false
	}

	switch status.Vendor {
	case vendorClaude, vendorAGY, vendorCodex:
	default:
		return false
	}

	switch status.Reason {
	case reasonNone:
		return status.Phase == phaseRunning
	case "login_required", "trust_required", "permission_required", processGone, ownershipLost,
		refreshTimeout, unknownScreen, retryExhausted:
		return status.Phase == phaseStuck
	case "rate_limited", humanAttached, readingUnavailable, unsupportedVersion, budgetExhausted,
		nearLimit, operatorStop, stateInvalid:
		return status.Phase == phaseStopped
	default:
		return false
	}
}

func validState(state State, enrollment Enrollment, now time.Time) bool {
	if !validStatus(state.Status) || !validEnrollment(state.Enrollment, []string{enrollment.Alias}) {
		return false
	}

	if state.Status.Alias != enrollment.Alias || state.Status.Vendor != enrollment.Vendor ||
		state.Enrollment.Vendor != enrollment.Vendor ||
		state.Enrollment.DebitAllowance != enrollment.DebitAllowance ||
		!slices.Equal(state.Enrollment.Windows, enrollment.Windows) {
		return false
	}

	if state.LastAt.IsZero() || now.Before(state.LastAt) || state.Status.At.After(state.LastAt) ||
		state.Retry < 0 || state.Retry > 3 ||
		state.InputCount != uint64(len(state.Attempts)) {
		return false
	}

	return validAttempts(state)
}

func validAttempts(state State) bool {
	previous := time.Time{}

	for _, attempt := range state.Attempts {
		if attempt.At.IsZero() || attempt.At.Before(previous) || attempt.At.After(state.LastAt) ||
			!ratio(attempt.Debit) {
			return false
		}

		previous = attempt.At
	}

	return state.PendingAt.IsZero() || state.PendingAt.Equal(previous)
}

func openSpool(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, ErrStateInvalid
	}

	resolved, err := filepath.EvalSymlinks(dir)
	// gomutants:disable-next-line BRANCH_IF reason="Resolution errors also fail the canonical path comparison."
	if err != nil {
		return nil, ErrStateInvalid
	}

	if resolved != filepath.Clean(dir) {
		return nil, ErrStateInvalid
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrStateInvalid
	}

	return root, nil
}

func regular(root spoolFiles, name string) (fs.FileInfo, error) {
	if filepath.Base(name) != name {
		return nil, ErrStateInvalid
	}

	info, err := root.Lstat(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // Store methods replace all filesystem errors with a fixed code.
	}

	if !info.Mode().IsRegular() {
		return nil, ErrStateInvalid
	}

	return info, nil
}

func read(root spoolFiles, name string, value any) error {
	info, err := regular(root, name)
	if err != nil {
		return ErrStateInvalid
	}

	file, err := root.Open(name)
	if err != nil {
		return ErrStateInvalid
	}

	defer file.Close()

	opened, err := file.Stat()
	// gomutants:disable-next-line BRANCH_IF reason="Nil Stat info also fails SameFile."
	if err != nil {
		return ErrStateInvalid
	}

	if !os.SameFile(info, opened) {
		return ErrStateInvalid
	}

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()

	if decoder.Decode(value) != nil {
		return ErrStateInvalid
	}

	if decoder.Decode(new(any)) != io.EOF {
		return ErrStateInvalid
	}

	return nil
}

func write(root spoolFiles, name string, value any, initial bool) error {
	_, err := regular(root, name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ErrStateInvalid
	}

	data, err := json.Marshal(value)
	if err != nil {
		return ErrStateInvalid
	}

	file, err := root.OpenFile(".keepalive-"+rand.Text(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return ErrStateInvalid
	}

	return replace(root, file, name, data, initial)
}

func replace(root spoolFiles, file *os.File, name string, data []byte, initial bool) error {
	temporary := filepath.Base(file.Name())

	defer root.Remove(temporary) //nolint:errcheck // A rename can consume the temporary file.

	_, writeErr := file.Write(data)

	if errors.Join(writeErr, file.Sync(), file.Close()) != nil {
		return ErrStateInvalid
	}

	var err error

	if initial {
		err = root.Link(temporary, name)
	} else {
		err = root.Rename(temporary, name)
	}

	if err != nil {
		return ErrStateInvalid
	}

	directory, err := root.Open(".")
	// gomutants:disable-next-line BRANCH_IF reason="A nil directory also fails Sync."
	if err != nil {
		return ErrStateInvalid
	}

	if errors.Join(directory.Sync(), directory.Close()) != nil {
		return ErrStateInvalid
	}

	return nil
}

// ReadStatus reads only a validated status record for a generic alias.
func (store Store) ReadStatus(alias string) (Status, error) {
	// gomutants:disable-next-line BRANCH_IF reason="Status validation and matching also reject invalid aliases."
	if !validAlias(alias) {
		return Status{}, ErrStateInvalid
	}

	root, err := openSpool(store.Dir)
	if err != nil {
		return Status{}, ErrStateInvalid
	}

	defer root.Close()

	var status Status

	if read(root, alias+".status", &status) != nil || !validStatus(status) || status.Alias != alias {
		return Status{}, ErrStateInvalid
	}

	return status, nil
}

// WriteStatus atomically replaces a status record after validation.
func (store Store) WriteStatus(status Status) error {
	if !validStatus(status) {
		return ErrStateInvalid
	}

	root, err := openSpool(store.Dir)
	if err != nil {
		return ErrStateInvalid
	}

	defer root.Close()

	return write(root, status.Alias+".status", status, false)
}

func (store Store) load(enrollment Enrollment, now time.Time) (State, error) {
	root, err := openSpool(store.Dir)
	if err != nil {
		return State{}, ErrStateInvalid
	}

	defer root.Close()

	var state State

	if read(root, enrollment.Alias+".ledger", &state) != nil || !validState(state, enrollment, now) {
		return State{}, ErrStateInvalid
	}

	return state, nil
}

func (store Store) save(state State, initial bool) error {
	if !validState(state, state.Enrollment, state.LastAt) {
		return ErrStateInvalid
	}

	root, err := openSpool(store.Dir)
	if err != nil {
		return ErrStateInvalid
	}

	defer root.Close()

	if initial {
		_, statErr := regular(root, state.Status.Alias+".status")

		if !errors.Is(statErr, fs.ErrNotExist) {
			return ErrStateInvalid
		}
	} else {
		_, statErr := regular(root, state.Status.Alias+".ledger")
		if statErr != nil {
			return ErrStateInvalid
		}
	}

	if write(root, state.Status.Alias+".ledger", state, initial) != nil {
		return ErrStateInvalid
	}

	return store.WriteStatus(state.Status)
}

func (store Store) stopped(alias string) (bool, error) {
	root, err := openSpool(store.Dir)
	if err != nil {
		return false, ErrStateInvalid
	}

	defer root.Close()

	for _, name := range []string{"stop", alias + ".stop"} {
		_, statErr := regular(root, name)
		if statErr == nil {
			return true, nil
		}

		if !errors.Is(statErr, fs.ErrNotExist) {
			return false, ErrStateInvalid
		}
	}

	return false, nil
}
