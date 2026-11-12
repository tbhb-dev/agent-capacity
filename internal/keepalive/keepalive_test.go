// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//nolint:testpackage // Fault fixtures exercise internal filesystem errors and pure guards.
package keepalive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sourceFunc func(context.Context, time.Time) (Snapshot, error)

func (read sourceFunc) Read(ctx context.Context, now time.Time) (Snapshot, error) {
	return read(ctx, now)
}

type observerFunc func(context.Context, time.Time) (Observation, error)

func (observe observerFunc) Observe(ctx context.Context, now time.Time) (Observation, error) {
	return observe(ctx, now)
}

func observationAt(at time.Time, err error) observerFunc {
	return func(context.Context, time.Time) (Observation, error) {
		return Observation{At: at, Signals: nil}, err
	}
}

func fixtureKeepalive(t *testing.T, now *time.Time) *Keepalive {
	t.Helper()

	keepalive, err := New(
		Store{Dir: spoolDir(t)},
		enrollment(),
		[]string{fixtureWork},
		func() time.Time { return *now },
		sourceFunc(func(ctx context.Context, at time.Time) (Snapshot, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, *now, at)

			return snapshotAt(at), nil
		}),
		observerFunc(func(ctx context.Context, at time.Time) (Observation, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, *now, at)

			return Observation{At: at, Signals: nil}, nil
		}),
	)
	require.NoError(t, err)

	return keepalive
}

func enrolledKeepalive(t *testing.T, now *time.Time) *Keepalive {
	t.Helper()

	keepalive := fixtureKeepalive(t, now)
	require.NoError(t, keepalive.Enroll())

	return keepalive
}

func step(t *testing.T, keepalive *Keepalive, wantReason, wantAction string) Decision {
	t.Helper()

	status, decision, err := keepalive.Step(t.Context(), false)
	require.NoError(t, err)
	assert.Equal(t, wantReason, status.Reason)
	assert.Equal(t, wantAction, decision.Action)

	stored, err := keepalive.store.ReadStatus(keepalive.enrollment.Alias)
	require.NoError(t, err)
	assert.Equal(t, status, stored)

	return decision
}

func invalidStep(t *testing.T, keepalive *Keepalive) {
	t.Helper()

	status, decision, err := keepalive.Step(t.Context(), false)
	require.ErrorIs(t, err, ErrStateInvalid)
	assert.Equal(t, fixtureStateInvalid, status.Reason)
	assert.Equal(t, phaseStopped, status.Phase)
	assert.Equal(t, fixtureNone, decision.Action)
	assert.True(t, decision.CheckAt.IsZero())
}

func setSpoolMode(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(dir, mode))
}

func TestInvalidLatchAfterSpoolRecovery(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	dir := keepalive.store.Dir

	t.Cleanup(func() { setSpoolMode(t, dir, 0o700) })

	keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
		setSpoolMode(t, dir, 0o500)

		return snapshotAt(now), nil
	})

	invalidStep(t, keepalive)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, fixtureNone, state.Status.Reason)
	assert.Zero(t, state.InputCount)

	setSpoolMode(t, dir, 0o700)

	keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
		return snapshotAt(now), nil
	})

	invalidStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestInvalidLatchAfterClockRecovery(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	now = now.Add(-time.Nanosecond)

	invalidStep(t, keepalive)

	now = instant()

	invalidStep(t, keepalive)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, fixtureStateInvalid, state.Status.Reason)
	assert.Zero(t, state.InputCount)

	restarted, err := New(
		keepalive.store,
		enrollment(),
		[]string{fixtureWork},
		keepalive.now,
		keepalive.source,
		keepalive.observer,
	)
	require.NoError(t, err)

	now = now.Add(8 * 24 * time.Hour)

	invalidStep(t, restarted)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)

	invalidStep(t, restarted)
	require.NoError(t, restarted.Rearm())
	step(t, restarted, fixtureBudgetExhausted, fixtureNone)
}

func TestFailedRearmPreservesInvalidLatch(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	now = now.Add(-time.Nanosecond)

	invalidStep(t, keepalive)

	now = instant()
	dir := keepalive.store.Dir

	t.Cleanup(func() { setSpoolMode(t, dir, 0o700) })
	setSpoolMode(t, dir, 0o500)
	require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)
	setSpoolMode(t, dir, 0o700)
	invalidStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestRestartAndRearm(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)
	step(t, keepalive, fixtureNone, fixtureAdmit)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), state.InputCount)
	assert.Equal(t, []Attempt{{At: now, Debit: 0.1}}, state.Attempts)

	// A new instance reads the pending attempt instead of repeating it.
	restarted, err := New(
		keepalive.store,
		enrollment(),
		[]string{fixtureWork},
		keepalive.now,
		keepalive.source,
		keepalive.observer,
	)
	require.NoError(t, err)
	step(t, restarted, fixtureNone, fixtureWait)

	now = now.Add(time.Second)

	step(t, restarted, fixtureNone, fixtureWait)

	state, err = restarted.store.load(restarted.enrollment, now)
	require.NoError(t, err)
	assert.True(t, state.PendingAt.IsZero())
	assert.Equal(t, uint64(1), state.InputCount)

	now = now.Add(5 * time.Minute)

	step(t, restarted, fixtureBudgetExhausted, fixtureNone)

	now = now.Add(8 * 24 * time.Hour)

	step(t, restarted, fixtureBudgetExhausted, fixtureNone)
	require.NoError(t, restarted.Rearm())
	step(t, restarted, fixtureNone, fixtureAdmit)

	state, err = restarted.store.load(restarted.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), state.InputCount)
	assert.Len(t, state.Attempts, 2)
	assert.Equal(t, instant(), state.Attempts[0].At)
}

func TestStopControls(t *testing.T) {
	t.Parallel()

	for _, name := range []string{fixtureStop, "work.stop"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			require.NoError(t, keepalive.Enroll())

			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)

			state.DueAt = now.Add(time.Hour)
			require.NoError(t, keepalive.store.save(state, false))

			decision := step(t, keepalive, fixtureNone, fixtureWait)
			assert.Equal(t, now.Add(time.Second), decision.CheckAt)
			require.NoError(t, os.WriteFile(filepath.Join(keepalive.store.Dir, name), nil, 0o600))

			now = decision.CheckAt
			decision = step(t, keepalive, fixtureOperatorStop, fixtureNone)
			assert.True(t, decision.CheckAt.IsZero())
			require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)
			require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, name)))
			step(t, keepalive, fixtureOperatorStop, fixtureNone)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}

	for _, cancelContext := range []bool{false, true} {
		now := instant()
		keepalive := fixtureKeepalive(t, &now)
		require.NoError(t, keepalive.Enroll())

		ctx, cancel := context.WithCancel(t.Context())

		if cancelContext {
			cancel()
		}

		status, decision, err := keepalive.Step(ctx, !cancelContext)

		cancel()
		require.NoError(t, err)
		assert.Equal(t, fixtureOperatorStop, status.Reason)
		assert.Equal(t, fixtureNone, decision.Action)
	}
}

func TestStopBeforeAdmission(t *testing.T) {
	t.Parallel()

	for _, action := range []string{fixtureStop, fixtureCancel, "lose ledger", "bad stop", "bad status", "bad directory"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)

			ctx, cancel := context.WithCancel(t.Context())

			defer cancel()

			keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
				switch action {
				case fixtureStop:
					require.NoError(t, os.WriteFile(filepath.Join(keepalive.store.Dir, fixtureStop), nil, 0o600))
				case fixtureCancel:
					cancel()
				case "lose ledger":
					require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.ledger")))
				case "bad stop":
					require.NoError(t, os.Mkdir(filepath.Join(keepalive.store.Dir, fixtureStop), 0o700))
				case "bad status":
					require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.status")))
					require.NoError(t, os.Mkdir(filepath.Join(keepalive.store.Dir, "work.status"), 0o700))
				case "bad directory":
					require.NoError(t, os.RemoveAll(keepalive.store.Dir))
				}

				return snapshotAt(now), nil
			})
			keepalive.observer = observationAt(now, nil)

			status, decision, err := keepalive.Step(ctx, false)

			if action == fixtureStop || action == fixtureCancel {
				require.NoError(t, err)
				assert.Equal(t, fixtureOperatorStop, status.Reason)

				state, loadErr := keepalive.store.load(keepalive.enrollment, now)
				require.NoError(t, loadErr)
				assert.Equal(t, uint64(1), state.InputCount)
			} else {
				require.ErrorIs(t, err, ErrStateInvalid)
				assert.Equal(t, fixtureStateInvalid, status.Reason)
			}

			assert.Equal(t, fixtureNone, decision.Action)
		})
	}
}

func TestStopStateErrorSurvivesRestart(t *testing.T) {
	t.Parallel()

	for _, signals := range [][]string{{"rate_limit"}, nil} {
		t.Run(strings.Join(signals, " "), func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			stopPath := filepath.Join(keepalive.store.Dir, fixtureStop)
			keepalive.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
				require.NoError(t, os.Mkdir(stopPath, 0o700))

				return Observation{At: now, Signals: signals}, nil
			})

			status, decision, err := keepalive.Step(t.Context(), false)
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
			assert.Equal(t, phaseStopped, status.Phase)
			assert.Equal(t, fixtureNone, decision.Action)

			restarted, err := New(keepalive.store, enrollment(), []string{fixtureWork}, keepalive.now,
				keepalive.source, observationAt(now, nil))
			require.NoError(t, err)
			require.NoError(t, os.Remove(stopPath))
			invalidStep(t, keepalive)

			for _, elapsed := range []time.Duration{0, 8 * 24 * time.Hour} {
				now = instant().Add(elapsed)

				status, decision, err = restarted.Step(t.Context(), false)
				require.ErrorIs(t, err, ErrStateInvalid)
				assert.Equal(t, phaseStopped, status.Phase)
				assert.Equal(t, fixtureNone, decision.Action)

				stored, statusErr := restarted.store.ReadStatus(fixtureWork)
				require.NoError(t, statusErr)
				assert.Equal(t, status, stored)
			}

			require.NoError(t, restarted.Rearm())

			restarted.observer = observationAt(now, nil)
			step(t, restarted, fixtureNone, fixtureAdmit)
		})
	}
}

func TestRuntimeObservations(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name        string
		want        string
		sourceError error
		observation Observation
	}{
		{"future", fixtureStateInvalid, nil, Observation{At: instant().Add(time.Nanosecond), Signals: nil}},
		{"zero", fixtureUnknownScreen, nil, Observation{At: time.Time{}, Signals: nil}},
		{"old", fixtureUnknownScreen, nil, Observation{At: instant().Add(-time.Second - time.Nanosecond), Signals: nil}},
		{"one second", fixtureNone, nil, Observation{At: instant().Add(-time.Second), Signals: nil}},
		{"dialog", fixtureUnknownScreen, nil, Observation{At: instant(), Signals: []string{"unknown_dialog"}}},
		{"reading error", "reading_unavailable", errors.ErrUnsupported, Observation{At: instant(), Signals: nil}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)

			keepalive.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
				return fixture.observation, nil
			})
			keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
				return snapshotAt(now), fixture.sourceError
			})

			wantAction := fixtureNone

			if fixture.want == fixtureNone {
				wantAction = fixtureAdmit
			}

			if fixture.want == fixtureStateInvalid {
				state, err := keepalive.store.load(keepalive.enrollment, now)
				require.NoError(t, err)

				step(t, keepalive, fixture.want, wantAction)
				require.NoError(t, keepalive.store.save(state, false))
				invalidStep(t, keepalive)
			} else {
				step(t, keepalive, fixture.want, wantAction)
			}
		})
	}
}

func TestPersistedRetries(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)

	keepalive.observer = observationAt(time.Time{}, errors.ErrUnsupported)

	for index, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		step(t, keepalive, fixtureNone, fixtureWait)

		state, err := keepalive.store.load(keepalive.enrollment, now)
		require.NoError(t, err)
		assert.Equal(t, now.Add(delay), state.DueAt)
		assert.Equal(t, index+1, state.Retry)
		assert.Zero(t, state.InputCount)

		restarted, newErr := New(
			keepalive.store,
			enrollment(),
			[]string{fixtureWork},
			keepalive.now,
			keepalive.source,
			keepalive.observer,
		)
		require.NoError(t, newErr)

		keepalive = restarted
		now = state.DueAt.Add(-time.Nanosecond)

		step(t, keepalive, fixtureNone, fixtureWait)

		now = state.DueAt
	}

	step(t, keepalive, "retry_exhausted", fixtureNone)
	require.NoError(t, keepalive.Rearm())

	keepalive.observer = observationAt(now, nil)
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestPendingTimeoutAndStateLoss(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"timeout", "rollback", fixtureMissing, "corrupt", "stop directory"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			require.NoError(t, keepalive.Enroll())
			step(t, keepalive, fixtureNone, fixtureAdmit)

			switch action {
			case "timeout":
				now = now.Add(120 * time.Second)

				step(t, keepalive, fixtureRefreshTimeout, fixtureNone)

				return
			case "rollback":
				now = now.Add(-time.Nanosecond)
			case fixtureMissing:
				require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.ledger")))
			case "corrupt":
				require.NoError(t, os.WriteFile(filepath.Join(keepalive.store.Dir, "work.ledger"), []byte("{"), 0o600))
			case "stop directory":
				require.NoError(t, os.Mkdir(filepath.Join(keepalive.store.Dir, "work.stop"), 0o700))
			}

			status, decision, err := keepalive.Step(t.Context(), false)
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
			assert.Equal(t, "stopped", status.Phase)
			assert.Equal(t, fixtureNone, decision.Action)

			stored, statusErr := keepalive.store.ReadStatus(fixtureWork)
			require.NoError(t, statusErr)
			assert.Equal(t, fixtureStateInvalid, stored.Reason)
			require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)
		})
	}
}

func TestEnrollment(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	copyEnrollment := enrollment()
	created, err := New(
		keepalive.store,
		copyEnrollment,
		[]string{fixtureWork},
		keepalive.now,
		keepalive.source,
		keepalive.observer,
	)
	require.NoError(t, err)

	copyEnrollment.Windows[0] = fixtureOther
	assert.Equal(t, "5h", created.enrollment.Windows[0])

	for _, fixture := range []struct {
		clock    func() time.Time
		source   Source
		observer Observer
		allow    []string
	}{
		{keepalive.now, keepalive.source, keepalive.observer, []string{fixturePersonal}},
		{keepalive.now, keepalive.source, keepalive.observer, nil},
		{nil, keepalive.source, keepalive.observer, []string{fixtureWork}},
		{keepalive.now, nil, keepalive.observer, []string{fixtureWork}},
		{keepalive.now, keepalive.source, nil, []string{fixtureWork}},
	} {
		created, err = New(
			keepalive.store,
			enrollment(),
			fixture.allow,
			fixture.clock,
			fixture.source,
			fixture.observer,
		)
		require.ErrorIs(t, err, ErrStateInvalid)
		assert.Nil(t, created)
	}

	status, decision, err := keepalive.Step(t.Context(), false)
	require.ErrorIs(t, err, ErrStateInvalid)
	assert.Equal(t, fixtureStateInvalid, status.Reason)
	assert.Equal(t, fixtureNone, decision.Action)

	now = time.Time{}

	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)
}

func TestOperatorLatchSurvivesTimeout(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	step(t, keepalive, fixtureNone, fixtureAdmit)

	status, decision, err := keepalive.Step(t.Context(), true)
	require.NoError(t, err)
	assert.Equal(t, fixtureOperatorStop, status.Reason)
	assert.Equal(t, fixtureNone, decision.Action)

	now = now.Add(120 * time.Second)

	step(t, keepalive, fixtureOperatorStop, fixtureNone)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureBudgetExhausted, fixtureNone)
}

func TestEnrollmentCannotReplaceLostLedger(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	step(t, keepalive, fixtureNone, fixtureAdmit)
	require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.ledger")))
	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)

	_, err := keepalive.store.load(keepalive.enrollment, now)
	require.ErrorIs(t, err, ErrStateInvalid)
}

func TestStopPreventsObservation(t *testing.T) {
	t.Parallel()

	for _, invalid := range []bool{false, true} {
		now := instant()
		keepalive := fixtureKeepalive(t, &now)
		require.NoError(t, keepalive.Enroll())

		name := filepath.Join(keepalive.store.Dir, fixtureStop)

		if invalid {
			require.NoError(t, os.Mkdir(name, 0o700))
		} else {
			require.NoError(t, os.WriteFile(name, nil, 0o600))
		}

		observed := 0

		keepalive.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
			observed++

			return Observation{At: now, Signals: nil}, nil
		})

		now = now.Add(time.Second)

		status, decision, err := keepalive.Step(t.Context(), false)

		if invalid {
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
		} else {
			require.NoError(t, err)
			assert.Equal(t, fixtureOperatorStop, status.Reason)
		}

		assert.Equal(t, fixtureNone, decision.Action)
		assert.Zero(t, observed)

		state, loadErr := keepalive.store.load(keepalive.enrollment, now)
		require.NoError(t, loadErr)
		assert.Zero(t, state.InputCount)
		assert.Equal(t, status, state.Status)
		assert.Equal(t, now, state.LastAt)
	}
}

func TestTimeoutDuringRetryWait(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	step(t, keepalive, fixtureNone, fixtureAdmit)

	keepalive.observer = observationAt(time.Time{}, errors.ErrUnsupported)

	now = instant().Add(time.Second)

	step(t, keepalive, fixtureNone, fixtureWait)

	now = now.Add(30 * time.Second)

	step(t, keepalive, fixtureNone, fixtureWait)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, instant().Add(151*time.Second), state.DueAt)

	now = instant().Add(120 * time.Second)

	step(t, keepalive, fixtureRefreshTimeout, fixtureNone)
}

func TestZeroClockAndObservation(t *testing.T) {
	t.Parallel()

	now := time.Time{}
	keepalive := fixtureKeepalive(t, &now)
	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)

	entries, err := os.ReadDir(keepalive.store.Dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	now = now.Add(time.Second)

	require.NoError(t, keepalive.Enroll())

	keepalive.observer = observationAt(time.Time{}, nil)

	step(t, keepalive, fixtureUnknownScreen, fixtureNone)
}
