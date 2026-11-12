// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package capacity_test

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.tbhb.dev/agent-capacity/internal/capacity"
)

const (
	codexSource  = "codex_rollout"
	unknownLabel = "unknown"
	agySource    = "agy_statusline"
	claudeSource = "claude_statusline"
	fresh        = "fresh"
	carried      = "carried"
	invalidClass = "invalid"
)

func reading() capacity.Reading {
	return capacity.Reading{
		Vendor: "anthropic", Window: "5h", Account: "work", Source: claudeSource,
		UsedRatio: 0.25, ResetAt: time.Time{}, ObservedAt: time.Unix(100000, 0),
	}
}

//nolint:dupl // Each label field has its own valid and invalid cases.
func TestValidate(t *testing.T) {
	t.Parallel()

	for _, account := range []string{"a", "0", "work_2-a", strings.Repeat("a", 32)} {
		r := reading()
		r.Account = account
		require.NoError(t, r.Validate())
	}

	for _, account := range []string{
		"", "@", "work@", "UPPER", "-work", "_work", "a.b", "a b", "é", strings.Repeat("a", 33),
	} {
		r := reading()
		r.Account = account
		require.ErrorIs(t, r.Validate(), capacity.ErrInvalidReading)
	}

	for _, ratio := range []float64{0, 1, 0.5} {
		r := reading()
		r.UsedRatio = ratio
		require.NoError(t, r.Validate())
	}

	for _, ratio := range []float64{-0.001, 1.001, math.NaN(), math.Inf(-1), math.Inf(1)} {
		r := reading()
		r.UsedRatio = ratio
		require.ErrorIs(t, r.Validate(), capacity.ErrInvalidReading)
	}

	for _, vendor := range []string{"anthropic", "openai", "google", "", unknownLabel} {
		r := reading()
		r.Vendor = vendor
		assert.Equal(t, vendor == "" || vendor == unknownLabel, errors.Is(r.Validate(), capacity.ErrInvalidReading))
	}

	for _, window := range []string{"5h", "7d", "7d_opus", "7d_sonnet", "other", "", unknownLabel} {
		r := reading()
		r.Window = window
		assert.Equal(t, window == "" || window == unknownLabel, errors.Is(r.Validate(), capacity.ErrInvalidReading))
	}

	for _, source := range []string{claudeSource, capacity.ClaudeTranscript, codexSource, agySource, "", unknownLabel} {
		r := reading()
		r.Source = source
		assert.Equal(t, source == "" || source == unknownLabel, errors.Is(r.Validate(), capacity.ErrInvalidReading))
	}
}

func TestFreshness(t *testing.T) {
	t.Parallel()

	cases := []struct {
		source string
		want   string
		age    time.Duration
	}{
		{age: -time.Second, source: claudeSource, want: fresh},
		{age: 0, source: claudeSource, want: fresh},
		{age: 10*time.Minute - time.Nanosecond, source: claudeSource, want: fresh},
		{age: 10 * time.Minute, source: claudeSource, want: fresh},
		{age: 10*time.Minute + time.Nanosecond, source: claudeSource, want: carried},
		{age: 10 * time.Minute, source: agySource, want: fresh},
		{age: 10*time.Minute + time.Nanosecond, source: agySource, want: carried},
		{age: 10*time.Minute + time.Second, source: codexSource, want: fresh},
		{age: 30*time.Minute - time.Nanosecond, source: codexSource, want: fresh},
		{age: 30 * time.Minute, source: codexSource, want: fresh},
		{age: 30*time.Minute + time.Nanosecond, source: codexSource, want: carried},
		{age: 6*time.Hour - time.Nanosecond, source: claudeSource, want: carried},
		{age: 6 * time.Hour, source: codexSource, want: carried},
		{age: 6*time.Hour + time.Nanosecond, source: agySource, want: "dropped"},
	}
	for _, testCase := range cases {
		r := reading()
		r.Source = testCase.source
		age, label := capacity.Freshness(r, r.ObservedAt.Add(testCase.age))
		assert.Equal(t, max(testCase.age, 0), age)
		assert.Equal(t, testCase.want, label)
	}
}

type sourceFunc func(context.Context, time.Time) ([]capacity.Reading, error)

func (f sourceFunc) Read(ctx context.Context, now time.Time) ([]capacity.Reading, error) {
	return f(ctx, now)
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	now := reading().ObservedAt
	invalid := reading()
	invalid.UsedRatio = -1

	cases := []struct {
		err       error
		name      string
		wantClass string
		batch     []capacity.Reading
	}{
		{name: claudeSource, batch: []capacity.Reading{reading()}, err: fs.ErrPermission, wantClass: "permission"},
		{
			name: claudeSource,
			batch: []capacity.Reading{
				reading(),
				{
					ResetAt:    time.Time{},
					ObservedAt: time.Time{},
					Vendor:     "",
					Window:     "",
					Account:    "",
					Source:     "",
					UsedRatio:  0,
				},
			},
			err:       nil,
			wantClass: invalidClass,
		},
		{name: claudeSource, batch: []capacity.Reading{reading(), invalid}, err: nil, wantClass: invalidClass},
		{name: agySource, batch: []capacity.Reading{reading()}, err: nil, wantClass: invalidClass},
		{name: unknownLabel, batch: nil, err: nil, wantClass: invalidClass},
		{name: claudeSource, batch: nil, err: nil, wantClass: ""},
		{name: capacity.ClaudeTranscript, batch: nil, err: nil, wantClass: ""},
		{name: claudeSource, batch: []capacity.Reading{reading()}, err: nil, wantClass: ""},
	}
	for _, testCase := range cases {
		valid := reading()
		valid.Source = codexSource
		registry := capacity.Registry{
			{
				Name: testCase.name,
				Source: sourceFunc(func(ctx context.Context, got time.Time) ([]capacity.Reading, error) {
					assert.NotEqual(t, unknownLabel, testCase.name)
					assert.Equal(t, t.Context(), ctx)
					assert.Equal(t, now, got)

					return testCase.batch, testCase.err
				}),
			},
			{Name: codexSource, Source: sourceFunc(func(context.Context, time.Time) ([]capacity.Reading, error) {
				return []capacity.Reading{valid}, nil
			})},
		}

		got, failures := registry.Read(t.Context(), now)
		if testCase.wantClass != "" {
			assert.Equal(t, []capacity.Reading{valid}, got)
			require.Len(t, failures, 1)
			assert.Equal(t, testCase.wantClass, failures[0].Class)
			wantSource := testCase.name
			assert.Equal(t, wantSource, failures[0].Source)
		} else {
			assert.Equal(t, append(testCase.batch, valid), got)
			assert.Empty(t, failures)
		}
	}

	got, failures := capacity.Registry(nil).Read(t.Context(), now)
	assert.Empty(t, got)
	assert.Empty(t, failures)
}

func TestErrorClass(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		err  error
		want string
	}{
		{capacity.ErrInvalidReading, invalidClass},
		{fs.ErrPermission, "permission"},
		{fs.ErrNotExist, "missing"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.ErrUnsupported, "read"},
	} {
		assert.Equal(t, testCase.want, capacity.ErrorClass(testCase.err))
	}
}
