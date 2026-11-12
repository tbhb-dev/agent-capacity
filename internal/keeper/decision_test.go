// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//nolint:testpackage // Fault fixtures exercise internal filesystem errors and pure guards.
package keeper

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fixtureRefreshTimeout  = "refresh_timeout"
	fixtureA               = "a"
	fixtureClaude          = "claude"
	fixtureAgy             = "agy"
	fixtureCodex           = "codex"
	fixtureWork            = "work"
	fixtureOther           = "other"
	fixtureUnknown         = "unknown"
	fixtureStatus          = "status"
	fixturePersonal        = "personal"
	fixtureStuck           = "stuck"
	fixtureRunning         = "running"
	fixtureNone            = "none"
	fixtureFile            = "file"
	fixtureUntouched       = "untouched"
	fixtureStop            = "stop"
	fixtureStat            = "stat"
	fixtureOpen            = "open"
	fixtureDifferentFile   = "different file"
	fixtureClosedRead      = "closed read"
	fixtureLink            = "link"
	fixtureOperatorStop    = "operator_stop"
	fixtureObserve         = "observe"
	fixtureWait            = "wait"
	fixtureMissing         = "missing"
	fixtureReadingInvalid  = "reading_invalid"
	fixtureAdmit           = "admit"
	fixtureDebitUnknown    = "debit_unknown"
	fixtureCountCap        = "count_cap"
	fixtureBudgetExhausted = "budget_exhausted"
	fixtureStateInvalid    = "state_invalid"
	fixtureCancel          = "cancel"
	fixtureUnknownScreen   = "unknown_screen"
)

func instant() time.Time { return time.Unix(2000000, 0).UTC() }

func enrollment() Enrollment {
	return Enrollment{Alias: fixtureWork, Vendor: fixtureClaude, Windows: []string{"5h", "7d"}, DebitAllowance: 0.5}
}

func initialState() State {
	enrolled := enrollment()

	return State{
		Status: Status{
			Alias:  enrolled.Alias,
			Vendor: enrolled.Vendor,
			Phase:  fixtureRunning,
			Reason: fixtureNone,
			At:     instant(),
		},
		Enrollment: enrolled,
		Attempts:   nil,
		InputCount: 0,
		LastAt:     instant(),
		DueAt:      instant(),
		PendingAt:  time.Time{},
		Retry:      0,
	}
}

func snapshotAt(now time.Time) Snapshot {
	debit := 0.1

	return Snapshot{Windows: []Window{
		{Name: "5h", ObservedAt: now, UsedRatio: 0.2, Carried: false},
		{Name: "7d", ObservedAt: now, UsedRatio: 0.3, Carried: false},
	}, Debit: &debit}
}

func TestStopFixtures(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/stops.json")
	require.NoError(t, err)

	var fixtures [][3]string
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.Len(t, fixtures, 29)

	for _, row := range fixtures {
		t.Run(row[0], func(t *testing.T) {
			t.Parallel()

			phase, reason := Stop(row[0])
			assert.Equal(t, row[1], phase)
			assert.Equal(t, row[2], reason)

			status := initialState().Status
			status.Phase, status.Reason = phase, reason
			assert.True(t, validStatus(status))

			status.Phase = "invalid"
			assert.False(t, validStatus(status))
		})
	}

	phase, reason := Stop()
	assert.Equal(t, fixtureRunning, phase)
	assert.Equal(t, fixtureNone, reason)

	phase, reason = Stop("client_detached", "stop_signal", "login_challenge")
	assert.Equal(t, "stopped", phase)
	assert.Equal(t, fixtureOperatorStop, reason)
}

func TestSchedule(t *testing.T) {
	t.Parallel()

	for _, vendor := range []string{fixtureClaude, fixtureAgy, fixtureCodex} {
		state := initialState()
		state.Enrollment.Vendor = vendor
		state.Status.Vendor = vendor

		wantInterval := 5 * time.Minute

		if vendor == fixtureCodex {
			wantInterval = 15 * time.Minute
		}

		assert.Equal(t, wantInterval, interval(vendor))
		assert.Equal(t, Decision{Action: fixtureObserve, CheckAt: instant()}, Next(state, instant()))

		state.DueAt = instant().Add(wantInterval)
		assert.Equal(t, Decision{Action: fixtureWait, CheckAt: instant().Add(time.Second)}, Next(state, instant()))

		before := state.DueAt.Add(-time.Nanosecond)
		assert.Equal(t, Decision{Action: fixtureWait, CheckAt: state.DueAt}, Next(state, before))
		assert.Equal(t, Decision{Action: fixtureObserve, CheckAt: state.DueAt}, Next(state, state.DueAt))

		later := instant().Add(24 * time.Hour)
		assert.Equal(t, Decision{Action: fixtureObserve, CheckAt: later}, Next(state, later))

		state.Retry = 1
		after, decision := evaluate(state, later, nil, snapshotAt(later))
		assert.Equal(t, later.Add(wantInterval), after.DueAt)
		assert.Equal(t, fixtureWait, decision.Action)
		assert.Empty(t, after.Attempts)
		assert.Zero(t, after.Retry)
		assert.Equal(t, fixtureWait, Next(after, later).Action)
	}
}

func TestReadings(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name   string
		change func(*Snapshot)
		want   string
	}{
		{fixtureMissing, func(s *Snapshot) { s.Windows = s.Windows[:1] }, "reading_missing"},
		{"extra", func(s *Snapshot) { s.Windows = append(s.Windows, s.Windows[0]) }, "reading_missing"},
		{"duplicate", func(s *Snapshot) { s.Windows[1].Name = "5h" }, fixtureReadingInvalid},
		{fixtureUnknown, func(s *Snapshot) { s.Windows[0].Name = fixtureOther }, fixtureReadingInvalid},
		{"zero time", func(s *Snapshot) { s.Windows[0].ObservedAt = time.Time{} }, fixtureReadingInvalid},
		{"carried", func(s *Snapshot) { s.Windows[1].Carried = true }, fixtureReadingInvalid},
		{"future", func(s *Snapshot) { s.Windows[0].ObservedAt = instant().Add(time.Nanosecond) }, "future_observation"},
		{"negative", func(s *Snapshot) { s.Windows[0].UsedRatio = -0.01 }, fixtureReadingInvalid},
		{"over one", func(s *Snapshot) { s.Windows[0].UsedRatio = 1.01 }, fixtureReadingInvalid},
		{"not a number", func(s *Snapshot) { s.Windows[0].UsedRatio = math.NaN() }, fixtureReadingInvalid},
		{"positive infinity", func(s *Snapshot) { s.Windows[0].UsedRatio = math.Inf(1) }, fixtureReadingInvalid},
		{"negative infinity", func(s *Snapshot) { s.Windows[0].UsedRatio = math.Inf(-1) }, fixtureReadingInvalid},
		{"threshold", func(s *Snapshot) { s.Windows[1].UsedRatio = 0.85 }, "near_limit"},
		{"full", func(s *Snapshot) { s.Windows[0].UsedRatio = 1 }, "near_limit"},
		{"below threshold", func(s *Snapshot) { s.Windows[1].UsedRatio = math.Nextafter(0.85, 0) }, ""},
		{"empty usage", func(s *Snapshot) { s.Windows[0].UsedRatio = 0 }, ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			snapshot := snapshotAt(instant())
			fixture.change(&snapshot)
			assert.Equal(t, fixture.want, readingSignal(enrollment(), snapshot, instant()))

			state, decision := evaluate(initialState(), instant(), nil, snapshot)

			if fixture.want == "" {
				assert.Equal(t, fixtureAdmit, decision.Action)
			} else {
				phase, reason := Stop(fixture.want)
				assert.Equal(t, phase, state.Status.Phase)
				assert.Equal(t, reason, state.Status.Reason)
				assert.Equal(t, fixtureNone, decision.Action)
			}
		})
	}

	for _, vendor := range []string{fixtureClaude, fixtureAgy, fixtureCodex} {
		freshFor := 10 * time.Minute

		if vendor == fixtureCodex {
			freshFor = 30 * time.Minute
		}

		for _, age := range []time.Duration{freshFor - time.Nanosecond, freshFor, freshFor + time.Nanosecond} {
			window := snapshotAt(instant()).Windows[0]
			window.ObservedAt = instant().Add(-age)

			want := ""

			if age >= freshFor {
				want = "reading_stale"
			}

			assert.Equal(t, want, windowSignal(window, vendor, instant()))
		}
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name      string
		want      string
		ages      []time.Duration
		debit     float64
		allowance float64
	}{
		{"empty", "", nil, 0.1, 0.1},
		{"zero debit", "", nil, 0, 0},
		{"one debit", "", nil, 1, 1},
		{"over allowance", "debit_allowance", nil, 0.1, math.Nextafter(0.1, 0)},
		{"invalid debit", fixtureDebitUnknown, nil, -0.1, 1},
		{"nan debit", fixtureDebitUnknown, nil, math.NaN(), 1},
		{"excess debit", fixtureDebitUnknown, nil, 1.1, 1},
		{"recent attempt", fixtureCountCap, []time.Duration{5*time.Hour - time.Nanosecond}, 0, 1},
		{"five hour boundary", "", []time.Duration{5 * time.Hour}, 0, 1},
		{"weekly cap", fixtureCountCap, []time.Duration{6 * 24 * time.Hour, 5 * time.Hour}, 0, 1},
		{"week boundary", "", []time.Duration{7 * 24 * time.Hour, 5 * time.Hour}, 0, 1},
		{"week before", fixtureCountCap, []time.Duration{7*24*time.Hour - time.Nanosecond, 5 * time.Hour}, 0, 1},
		{"durable debit", "debit_allowance", []time.Duration{8 * 24 * time.Hour}, 0.1, 0.19},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			state := initialState()

			state.Enrollment.DebitAllowance = fixture.allowance

			for _, age := range fixture.ages {
				state.Attempts = append(state.Attempts, Attempt{At: instant().Add(-age), Debit: 0.1})
			}

			state.InputCount = uint64(len(state.Attempts))
			assert.Equal(t, fixture.want, budgetSignal(state, &fixture.debit, instant()))

			snapshot := snapshotAt(instant())
			snapshot.Debit = &fixture.debit

			after, decision := evaluate(state, instant(), nil, snapshot)

			if fixture.want == "" {
				assert.Equal(t, fixtureAdmit, decision.Action)
				assert.Equal(t, state.InputCount+1, after.InputCount)
			} else {
				assert.Equal(t, fixtureNone, decision.Action)
				assert.Equal(t, fixtureBudgetExhausted, after.Status.Reason)
				assert.Equal(t, state.Attempts, after.Attempts)
			}
		})
	}

	assert.Equal(t, fixtureDebitUnknown, budgetSignal(initialState(), nil, instant()))
}

func TestRefreshAndLatch(t *testing.T) {
	t.Parallel()

	state, decision := evaluate(initialState(), instant(), nil, snapshotAt(instant()))
	assert.Equal(t, fixtureAdmit, decision.Action)
	assert.Equal(t, []Attempt{{At: instant(), Debit: 0.1}}, state.Attempts)
	assert.Equal(t, instant(), state.PendingAt)
	assert.Equal(t, uint64(1), state.InputCount)

	for _, delay := range []time.Duration{time.Second, 120*time.Second - time.Nanosecond} {
		after, next := evaluate(state, instant().Add(delay), nil, snapshotAt(instant()))
		assert.Equal(t, fixtureRunning, after.Status.Phase)
		assert.Equal(t, fixtureWait, next.Action)
		assert.Equal(t, instant().Add(delay+time.Second), after.DueAt)
		assert.Equal(t, state.PendingAt, after.PendingAt)
		assert.Equal(t, state.Attempts, after.Attempts)
	}

	partial := snapshotAt(instant().Add(time.Second))
	partial.Windows[1].ObservedAt = instant()
	after, next := evaluate(state, instant().Add(time.Second), nil, partial)
	assert.Equal(t, state.PendingAt, after.PendingAt)
	assert.Equal(t, fixtureWait, next.Action)

	after, next = evaluate(state, instant().Add(time.Second), nil, snapshotAt(instant().Add(time.Second)))
	assert.True(t, after.PendingAt.IsZero())
	assert.Equal(t, fixtureWait, next.Action)
	assert.Equal(t, instant().Add(time.Second+5*time.Minute), after.DueAt)
	assert.Equal(t, state.Attempts, after.Attempts)

	for _, delay := range []time.Duration{120 * time.Second, 120*time.Second + time.Nanosecond} {
		after, next = evaluate(state, instant().Add(delay), nil, snapshotAt(instant().Add(delay)))
		assert.Equal(t, fixtureRefreshTimeout, after.Status.Reason)
		assert.Equal(t, fixtureNone, next.Action)
	}

	after, next = evaluate(initialState(), instant(), []string{"client_attached"}, snapshotAt(instant()))
	assert.Equal(t, "human_attached", after.Status.Reason)
	assert.Equal(t, fixtureNone, next.Action)

	latched, next := evaluate(after, instant().Add(time.Hour), []string{"client_detached"}, snapshotAt(instant()))
	assert.Equal(t, after, latched)
	assert.Equal(t, fixtureNone, next.Action)

	after, next = evaluate(
		initialState(),
		instant().Add(-time.Nanosecond),
		nil,
		snapshotAt(instant().Add(-time.Nanosecond)),
	)
	assert.Equal(t, fixtureStateInvalid, after.Status.Reason)
	assert.Equal(t, fixtureNone, next.Action)
}

func TestRetryDecisions(t *testing.T) {
	t.Parallel()

	state := initialState()

	now := instant()

	for index, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		state = retry(state, now)
		assert.Equal(t, index+1, state.Retry)
		assert.Equal(t, now.Add(delay), state.DueAt)
		assert.Equal(t, fixtureRunning, state.Status.Phase)
		assert.Empty(t, state.Attempts)

		now = state.DueAt
	}

	state = retry(state, now)
	assert.Equal(t, fixtureStuck, state.Status.Phase)
	assert.Equal(t, "retry_exhausted", state.Status.Reason)
	assert.True(t, state.DueAt.IsZero())
	assert.Equal(t, now, state.Status.At)
}

func TestAdmissionWithOneWindow(t *testing.T) {
	t.Parallel()

	state := initialState()
	state.Enrollment.Windows = []string{"5h"}
	snapshot := snapshotAt(instant())
	snapshot.Windows = snapshot.Windows[:1]
	after, decision := evaluate(state, instant(), nil, snapshot)
	assert.Equal(t, fixtureAdmit, decision.Action)
	assert.Equal(t, uint64(1), after.InputCount)

	window := snapshot.Windows[0]
	window.ObservedAt = time.Time{}
	assert.Equal(t, fixtureReadingInvalid, windowSignal(window, fixtureClaude, time.Time{}.Add(time.Second)))
}

func TestObservationUpdatesTimeAndRetry(t *testing.T) {
	t.Parallel()

	later := instant().Add(time.Second)
	signaled, _ := evaluate(initialState(), later, []string{"client_attached"}, snapshotAt(later))
	assert.Equal(t, later, signaled.Status.At)
	assert.Equal(t, later, signaled.LastAt)
	assert.True(t, signaled.DueAt.IsZero())

	state, decision := evaluate(initialState(), later, nil, snapshotAt(later))
	assert.Equal(t, fixtureAdmit, decision.Action)
	assert.Equal(t, later, state.LastAt)

	state.Retry = 1
	refreshed := later.Add(time.Second)
	after, next := evaluate(state, refreshed, nil, snapshotAt(refreshed))
	assert.True(t, after.PendingAt.IsZero())
	assert.Zero(t, after.Retry)
	assert.Equal(t, fixtureWait, next.Action)
	assert.Equal(t, state.Attempts, after.Attempts)
}
