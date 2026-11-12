// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package capacity

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// Source reads passive local inputs. No input returns nil, nil; unreadable or
// malformed input returns no readings and an error. Implementations live in
// internal/source/claude, internal/source/codex and internal/source/agy.
type Source interface {
	Read(ctx context.Context, now time.Time) ([]Reading, error)
}

// Registration binds a source to one of the reading's allowed source names.
type Registration struct {
	Source Source
	Name   string
}

// Registry reads sources in registration order. Its zero value has no sources.
type Registry []Registration

// Failure identifies a source and a safe error class, without input content.
type Failure struct {
	Source string
	Class  string
}

// Read rejects the entire batch on any error, including validation errors.
// Successful sources still contribute readings when another source fails.
func (r Registry) Read(ctx context.Context, now time.Time) ([]Reading, []Failure) {
	var (
		readings []Reading
		failures []Failure
	)

	for _, entry := range r {
		switch entry.Name {
		case ClaudeStatusline, CodexRollout, AGYStatusline:
		default:
			failures = append(failures, Failure{Source: "unknown", Class: "invalid"})

			continue
		}

		batch, err := entry.Source.Read(ctx, now)
		if err == nil {
			err = validateBatch(batch, entry.Name)
		}

		if err != nil {
			failures = append(failures, Failure{Source: entry.Name, Class: ErrorClass(err)})

			continue
		}

		readings = append(readings, batch...)
	}

	return readings, failures
}

func validateBatch(batch []Reading, name string) error {
	for _, reading := range batch {
		if reading.Source != name {
			return ErrInvalidReading
		}

		err := reading.Validate()
		if err != nil {
			return err
		}
	}

	return nil
}

// ErrorClass returns a fixed vocabulary without the error message.
func ErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrInvalidReading):
		return "invalid"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "read"
	}
}
