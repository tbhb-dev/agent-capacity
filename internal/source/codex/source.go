// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package codex reads plan snapshots from existing rollout tails.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	maxFiles     = 5
	chunkSize    = 64 * 1024
	maxBytes     = 8 * 1024 * 1024
	percentScale = 100
	shortMinutes = 300
	weekMinutes  = 10080
	seenCooldown = time.Hour
)

type seenEntry struct {
	loggedAt time.Time
	windows  []int
}

//nolint:gochecknoglobals // Value-receiver sources share per-account log deduplication across reads.
var (
	errRead      = errors.New("rollout read failed")
	rolloutPath  = regexp.MustCompile(`^sessions/[0-9]{4}/[0-9]{2}/[0-9]{2}/rollout-[^/]*\.jsonl$`)
	accountAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	seenLock     sync.Mutex
	seenAccounts = map[string]seenEntry{}
)

// Source reads only usage fields and never retains rollout content.
type Source struct {
	//exhaustruct:optional
	Logger  *slog.Logger
	files   fs.FS
	account string
}

// Register returns no source without an explicitly configured home.
func Register(home, account string, logger *slog.Logger) (capacity.Registry, error) {
	if !accountAlias.MatchString(account) {
		return nil, capacity.ErrInvalidReading
	}

	if home == "" {
		return nil, nil
	}

	return capacity.Registry{
		{Name: capacity.CodexRollout, Source: Source{Logger: logger, files: os.DirFS(home), account: account}},
	}, nil
}

type candidate struct {
	modified time.Time
	name     string
}

func newest(files fs.FS) ([]candidate, error) {
	var found []candidate

	err := fs.WalkDir(files, "sessions", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return safeError(walkErr)
		}

		if name == "sessions" && !entry.IsDir() {
			return errRead
		}

		if !rolloutPath.MatchString(name) || !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return safeError(err)
		}

		found = append(found, candidate{name: name, modified: info.ModTime()})
		slices.SortStableFunc(found, func(a, b candidate) int {
			return b.modified.Compare(a.modified)
		})
		found = found[:min(len(found), maxFiles)]

		return nil
	})
	if err != nil {
		return nil, safeError(err)
	}

	return found, nil
}

// Read chooses one complete snapshot by its event timestamp, across five tails.
func (s Source) Read(ctx context.Context, now time.Time) ([]capacity.Reading, error) {
	err := ctx.Err()
	if err != nil {
		return nil, safeError(err)
	}

	files, err := newest(s.files)
	if err != nil {
		return nil, safeError(err)
	}

	var latest snapshot
	for _, file := range files {
		err = s.readTail(ctx, file.name, &latest)
		if err != nil {
			return nil, safeError(err)
		}
	}

	windows, seen := latest.windows()
	logWindows(ctx, s.Logger, s.account, seen, now)

	readings, err := latest.readings(s.account, windows)
	if err != nil {
		return nil, err
	}

	remaining := int64(capacity.WindowTokenBytes)

	for _, label := range []string{"5h", "7d"} {
		for i := range readings {
			if readings[i].Window == label && !readings[i].ResetAt.IsZero() {
				readings[i].Tokens = s.windowTokens(ctx, now, readings[i], &remaining)
			}
		}
	}

	return readings, nil
}

func safeError(err error) error {
	switch capacity.ErrorClass(err) {
	case "missing":
		return nil
	case "permission":
		return fs.ErrPermission
	case "canceled":
		return context.Canceled
	case "timeout":
		return context.DeadlineExceeded
	default:
		return errRead
	}
}

func (s Source) readTail(ctx context.Context, name string, latest *snapshot) error {
	file, err := s.files.Open(name)
	if err != nil {
		return safeError(err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return safeError(err)
	}

	reader, ok := file.(io.ReaderAt)
	if !ok {
		return errRead
	}

	return scanTail(ctx, reader, info.Size(), latest)
}

func scanTail(ctx context.Context, reader io.ReaderAt, size int64, latest *snapshot) error {
	remaining := min(size, int64(maxBytes))
	offset := size
	buffer := make([]byte, remaining)
	end := remaining

	for remaining > 0 {
		err := ctx.Err()
		if err != nil {
			return safeError(err)
		}

		length := min(remaining, int64(chunkSize))
		offset -= length
		remaining -= length
		chunk := buffer[remaining : remaining+length]

		_, err = reader.ReadAt(chunk, offset)
		if err != nil {
			return safeError(err)
		}

		for {
			err = ctx.Err()
			if err != nil {
				return safeError(err)
			}

			index := bytes.LastIndexByte(buffer[remaining:end], '\n')
			if index < 0 {
				break
			}

			split := remaining + int64(index)
			latest.consider(buffer[split:end])
			end = split
		}
	}

	if offset == 0 {
		latest.consider(buffer[:end])
	}

	return nil
}

//nolint:tagliatelle // Rollout fields follow the existing JSON schema.
type window struct {
	UsedPercent   *float64 `json:"used_percent"`
	ResetsAt      *int64   `json:"resets_at"`
	WindowMinutes *int     `json:"window_minutes"`
}

//nolint:tagliatelle // Rollout fields follow the existing JSON schema.
type limits struct {
	LimitName *string `json:"limit_name"`
	Primary   *window `json:"primary"`
	Secondary *window `json:"secondary"`
}

//nolint:tagliatelle // Rollout fields follow the existing JSON schema.
type event struct {
	Timestamp *time.Time `json:"timestamp"`
	Payload   struct {
		RateLimits *limits `json:"rate_limits"`
		Type       string  `json:"type"`
	} `json:"payload"`
	Type string `json:"type"`
}

type snapshot struct {
	observed time.Time
	limits   *limits
}

func (s *snapshot) consider(line []byte) {
	var record event
	if json.Unmarshal(line, &record) != nil {
		return
	}

	if record.Type != "event_msg" || record.Payload.Type != "token_count" || record.Payload.RateLimits == nil ||
		record.Timestamp == nil {
		return
	}

	limits := record.Payload.RateLimits
	// The default family has a null name or the exact name Codex.
	if limits.LimitName != nil && *limits.LimitName != "Codex" {
		return
	}

	if s.limits == nil || record.Timestamp.After(s.observed) {
		s.observed = *record.Timestamp
		s.limits = limits
	}
}

func (s *snapshot) windows() ([]*window, []int) {
	if s.limits == nil {
		return nil, nil
	}

	var windows []*window

	var seen []int

	for _, window := range []*window{s.limits.Primary, s.limits.Secondary} {
		if window == nil || window.WindowMinutes == nil {
			continue
		}

		windows = append(windows, window)
		seen = append(seen, *window.WindowMinutes)
	}

	return windows, seen
}

func (s *snapshot) readings(account string, windows []*window) ([]capacity.Reading, error) {
	var readings []capacity.Reading

	for _, window := range windows {
		var label string

		switch *window.WindowMinutes {
		case shortMinutes:
			label = "5h"
		case weekMinutes:
			label = "7d"
		default:
			continue
		}

		if window.UsedPercent == nil {
			return nil, capacity.ErrInvalidReading
		}

		reading := capacity.Reading{
			Bucket: "", Vendor: "openai", Window: label, Account: account, Source: capacity.CodexRollout,
			UsedRatio: *window.UsedPercent / percentScale, ObservedAt: s.observed, ResetAt: time.Time{},
		}
		if window.ResetsAt != nil {
			reading.ResetAt = time.Unix(*window.ResetsAt, 0)
		}

		err := reading.Validate()
		if err != nil {
			return nil, capacity.ErrInvalidReading
		}

		readings = append(readings, reading)
	}

	return readings, nil
}

func logWindows(ctx context.Context, logger *slog.Logger, account string, seen []int, now time.Time) {
	if logger == nil || seen == nil {
		return
	}

	slices.Sort(seen)
	seen = slices.Compact(seen)

	seenLock.Lock()
	defer seenLock.Unlock()

	previous := seenAccounts[account]
	if slices.Equal(previous.windows, seen) && now.Before(previous.loggedAt.Add(seenCooldown)) {
		return
	}

	seenAccounts[account] = seenEntry{loggedAt: now, windows: seen}
	logger.InfoContext(ctx, "codex windows", "account", account, "windows_seen", seen)
}
