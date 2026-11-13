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
	require.NoError(t, keepalive.Rearm())

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

	failSaves(t, keepalive, now)

	invalidStep(t, keepalive)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, fixtureNone, state.Status.Reason)
	assert.Zero(t, state.InputCount)

	setSpoolMode(t, dir, 0o700)

	keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
		return snapshotAt(now), nil
	})

	unarmedStep(t, keepalive)
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

	unarmedStep(t, keepalive)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, fixtureNone, state.Status.Reason)
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

	unarmedStep(t, restarted)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)

	unarmedStep(t, restarted)
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
	unarmedStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestRestartAndRearm(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	require.NoError(t, keepalive.Rearm())
	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)
	unarmedStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), state.InputCount)
	assert.Equal(t, []Attempt{{At: now, Debit: 0.1}}, state.Attempts)

	// A new instance preserves history and needs its own Rearm.
	restarted := restartKeepalive(t, keepalive)
	unarmedStep(t, restarted)
	require.NoError(t, restarted.Rearm())
	step(t, restarted, fixtureBudgetExhausted, fixtureNone)
	unarmedStep(t, restarted)

	now = now.Add(8 * 24 * time.Hour)

	unarmedStep(t, restarted)
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

	for _, name := range []string{fixtureStop, fixtureAliasStop} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			require.NoError(t, keepalive.Enroll())
			require.NoError(t, keepalive.Rearm())

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
			unarmedStep(t, keepalive)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}

	for _, cancelContext := range []bool{false, true} {
		now := instant()
		keepalive := fixtureKeepalive(t, &now)
		require.NoError(t, keepalive.Enroll())
		require.NoError(t, keepalive.Rearm())

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

				if action == fixtureStop {
					require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, fixtureStop)))
				}

				require.NoError(t, keepalive.store.save(initialState(), false))
				unarmedStep(t, keepalive)
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
			unarmedStep(t, keepalive)

			for _, elapsed := range []time.Duration{0, 8 * 24 * time.Hour} {
				now = instant().Add(elapsed)

				unarmedStep(t, restarted)
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
				unarmedStep(t, keepalive)
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
	step(t, keepalive, fixtureNone, fixtureWait)

	state, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(30*time.Second), state.DueAt)
	assert.Equal(t, 1, state.Retry)
	assert.Zero(t, state.InputCount)
	unarmedStep(t, keepalive)

	restarted := restartKeepalive(t, keepalive)
	now = state.DueAt

	unarmedStep(t, restarted)
	require.NoError(t, restarted.Rearm())
	restarted.observer = observationAt(now, nil)
	step(t, restarted, fixtureNone, fixtureAdmit)
}

func TestPendingTimeoutAndStateLoss(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"timeout", "rollback", fixtureMissing, "corrupt", "stop directory"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			require.NoError(t, keepalive.Enroll())
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)

			switch action {
			case "timeout":
				now = now.Add(120 * time.Second)

				observed := 0
				keepalive.observer = countedObservation(&now, &observed)

				step(t, keepalive, fixtureRefreshTimeout, fixtureNone)
				assert.Zero(t, observed)

				return
			case "rollback":
				now = now.Add(-time.Nanosecond)
			case fixtureMissing:
				require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.ledger")))
			case "corrupt":
				require.NoError(t, os.WriteFile(filepath.Join(keepalive.store.Dir, "work.ledger"), []byte("{"), 0o600))
			case "stop directory":
				require.NoError(t, os.Mkdir(filepath.Join(keepalive.store.Dir, fixtureAliasStop), 0o700))
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

	unarmedStep(t, keepalive)

	now = time.Time{}

	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)
}

func TestOperatorLatchSurvivesTimeout(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)

	now = now.Add(120 * time.Second)

	status, decision, err := keepalive.Step(t.Context(), true)
	require.NoError(t, err)
	assert.Equal(t, fixtureOperatorStop, status.Reason)
	assert.Equal(t, fixtureNone, decision.Action)

	now = now.Add(120 * time.Second)

	unarmedStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureBudgetExhausted, fixtureNone)
}

func TestEnrollmentCannotReplaceLostLedger(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())
	require.NoError(t, keepalive.Rearm())
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
		require.NoError(t, keepalive.Rearm())

		name := filepath.Join(keepalive.store.Dir, fixtureStop)

		if invalid {
			require.NoError(t, os.Mkdir(name, 0o700))
		} else {
			require.NoError(t, os.WriteFile(name, nil, 0o600))
		}

		observed := 0

		keepalive.observer = countedObservation(&now, &observed)

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

	now = instant().Add(120 * time.Second)

	unarmedStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	keepalive.observer = observationAt(now, nil)
	step(t, keepalive, fixtureBudgetExhausted, fixtureNone)
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
	require.NoError(t, keepalive.Rearm())

	keepalive.observer = observationAt(time.Time{}, nil)

	step(t, keepalive, fixtureUnknownScreen, fixtureNone)
}

func restartKeepalive(t *testing.T, previous *Keepalive) *Keepalive {
	t.Helper()

	restarted, err := New(previous.store, previous.enrollment, []string{fixtureWork}, previous.now,
		previous.source, previous.observer)
	require.NoError(t, err)

	return restarted
}

func unarmedStep(t *testing.T, keepalive *Keepalive) {
	t.Helper()

	status, decision, err := keepalive.Step(t.Context(), false)
	require.NoError(t, err)
	assert.Equal(t, fixtureNotArmed, status.Reason)
	assert.Equal(t, phaseStopped, status.Phase)
	assert.Equal(t, fixtureNone, decision.Action)
	assert.True(t, decision.CheckAt.IsZero())
}

func TestStrictRestartAfterSaveFailures(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	require.NoError(t, keepalive.Rearm())
	dir := keepalive.store.Dir

	t.Cleanup(func() { setSpoolMode(t, dir, 0o700) })
	failSaves(t, keepalive, now)
	invalidStep(t, keepalive)
	setSpoolMode(t, dir, 0o700)

	keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
		return snapshotAt(now), nil
	})

	restarted := restartKeepalive(t, keepalive)
	unarmedStep(t, restarted)
	require.NoError(t, restarted.Rearm())
	step(t, restarted, fixtureNone, fixtureAdmit)
}

func TestStrictRestartAfterRollback(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	require.NoError(t, keepalive.Rearm())

	now = now.Add(-time.Nanosecond)

	invalidStep(t, keepalive)

	now = instant()

	unarmedStep(t, restartKeepalive(t, keepalive))
}

// A symlink makes Store refuse the status read and replacement.
func refuseStatusWrite(t *testing.T, keepalive *Keepalive) func() {
	t.Helper()

	path := filepath.Join(keepalive.store.Dir, "work.status")
	backup := path + ".saved"
	require.NoError(t, os.Rename(path, backup))
	require.NoError(t, os.Symlink(backup, path))

	return func() {
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Rename(backup, path))
	}
}

func TestStrictRearmPreservesPersistedStop(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	require.NoError(t, keepalive.Rearm())
	_, _, err := keepalive.Step(t.Context(), true)
	require.NoError(t, err)
	before, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	restore := refuseStatusWrite(t, keepalive)
	require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)
	after, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	restore()

	status, err := keepalive.store.ReadStatus(fixtureWork)
	require.NoError(t, err)
	assert.Equal(t, before.Status, status)
}

func TestStrictFailedRearmClosesGate(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{fixtureStop, fixtureStopRead, fixtureLedgerLoad, "status write"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			require.NoError(t, keepalive.Rearm())
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)

			path := filepath.Join(keepalive.store.Dir, fixtureStop)

			restore := func() { require.NoError(t, os.Remove(path)) }

			switch failure {
			case fixtureStop:
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			case fixtureStopRead:
				require.NoError(t, os.Mkdir(path, 0o700))
			case fixtureLedgerLoad:
				path = filepath.Join(keepalive.store.Dir, "work.ledger")
				require.NoError(t, os.Rename(path, path+".saved"))

				restore = func() { require.NoError(t, os.Rename(path+".saved", path)) }
			case "status write":
				restore = refuseStatusWrite(t, keepalive)
			}

			ledgerPath := filepath.Join(keepalive.store.Dir, "work.ledger")
			if failure == fixtureLedgerLoad {
				ledgerPath += ".saved"
			}

			before, err := os.Stat(ledgerPath)
			require.NoError(t, err)
			require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)

			after, err := os.Stat(ledgerPath)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after))
			restore()
			require.NoError(t, keepalive.store.save(state, false))
			unarmedStep(t, keepalive)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}
}

func TestStrictNewInstanceNeedsRearm(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := fixtureKeepalive(t, &now)
	require.NoError(t, keepalive.Enroll())

	state, err := keepalive.store.load(enrollment(), now)
	require.NoError(t, err)
	assert.Equal(t, initialState(), state)

	path := filepath.Join(keepalive.store.Dir, "work.ledger")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	unarmedStep(t, keepalive)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	later, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.True(t, os.SameFile(info, later))
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestStrictInternalInvalidBeforeStop(t *testing.T) {
	t.Parallel()

	for _, control := range []string{fixtureStop, fixtureCancel} {
		t.Run(control, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			require.NoError(t, keepalive.Rearm())
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			path := filepath.Join(keepalive.store.Dir, fixtureStop)
			keepalive.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
				if control == fixtureStop {
					require.NoError(t, os.WriteFile(path, nil, 0o600))
				} else {
					cancel()
				}

				return Observation{At: now.Add(time.Nanosecond), Signals: nil}, nil
			})
			status, decision, err := keepalive.Step(ctx, false)
			require.NoError(t, err)
			assert.Equal(t, fixtureOperatorStop, status.Reason)
			assert.Equal(t, fixtureNone, decision.Action)

			if control == fixtureStop {
				require.NoError(t, os.Remove(path))
			}

			require.NoError(t, keepalive.store.save(state, false))
			keepalive.observer = observationAt(now, nil)
			unarmedStep(t, keepalive)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}
}

func TestStrictSameInstanceAfterInternalError(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	require.NoError(t, keepalive.Rearm())

	now = now.Add(-time.Nanosecond)

	invalidStep(t, keepalive)

	now = instant()
	status, decision, err := keepalive.Step(t.Context(), false)
	require.NoError(t, err)
	assert.NotEqual(t, fixtureAdmit, decision.Action)
	assert.NotEqual(t, fixtureNone, status.Reason)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestStrictRearmRefusesStopFiles(t *testing.T) {
	t.Parallel()

	for _, name := range []string{fixtureStop, fixtureAliasStop} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			require.NoError(t, keepalive.Rearm())
			path := filepath.Join(keepalive.store.Dir, name)
			require.NoError(t, os.WriteFile(path, nil, 0o600))

			ledgerPath := filepath.Join(keepalive.store.Dir, "work.ledger")
			before, err := os.Stat(ledgerPath)
			require.NoError(t, err)
			require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)

			after, err := os.Stat(ledgerPath)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after))
			status, decision, err := keepalive.Step(t.Context(), false)
			require.NoError(t, err)
			assert.Equal(t, fixtureOperatorStop, status.Reason)
			assert.Equal(t, fixtureNone, decision.Action)
		})
	}
}

func failSaves(t *testing.T, keepalive *Keepalive, now time.Time) {
	t.Helper()

	keepalive.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
		setSpoolMode(t, keepalive.store.Dir, 0o500)

		return snapshotAt(now), nil
	})
}

const (
	fixtureClockRollback = "clock rollback"
	fixtureLedgerLoad    = "ledger load"
	fixtureReadError     = "read error"
	fixtureStopRead      = "stop read"
)

func TestRearmRechecks(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{fixtureStop, fixtureStopRead, fixtureClockRollback} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			calls := 0
			path := filepath.Join(keepalive.store.Dir, fixtureStop)
			keepalive.now = func() time.Time {
				calls++
				if calls == 2 {
					switch failure {
					case fixtureStop:
						require.NoError(t, os.WriteFile(path, nil, 0o600))
					case fixtureStopRead:
						require.NoError(t, os.Mkdir(path, 0o700))
					case fixtureClockRollback:
						return now.Add(-time.Nanosecond)
					}
				}

				return now
			}
			require.ErrorIs(t, keepalive.Rearm(), ErrStateInvalid)

			if failure != fixtureClockRollback {
				require.NoError(t, os.Remove(path))
			}

			unarmedStep(t, keepalive)
		})
	}
}

func TestBlockedStopControls(t *testing.T) {
	t.Parallel()

	for _, control := range []string{fixtureStop, fixtureCancel, fixtureReadError} {
		t.Run(control, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			require.NoError(t, keepalive.Enroll())

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if control == fixtureCancel {
				cancel()
			}

			if control == fixtureReadError {
				require.NoError(t, os.Mkdir(filepath.Join(keepalive.store.Dir, fixtureStop), 0o700))
			}

			status, decision, err := keepalive.Step(ctx, control == fixtureStop)
			if control == fixtureReadError {
				require.ErrorIs(t, err, ErrStateInvalid)
				assert.Equal(t, fixtureStateInvalid, status.Reason)
			} else {
				require.NoError(t, err)
				assert.Equal(t, fixtureOperatorStop, status.Reason)
			}

			assert.Equal(t, fixtureNone, decision.Action)
		})
	}
}

func TestInternalErrorStillHonorsStops(t *testing.T) {
	t.Parallel()

	for _, control := range []string{fixtureStop, fixtureCancel, "signal"} {
		t.Run(control, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			require.NoError(t, os.Remove(filepath.Join(keepalive.store.Dir, "work.ledger")))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			switch control {
			case fixtureStop:
				require.NoError(t, os.WriteFile(filepath.Join(keepalive.store.Dir, fixtureStop), nil, 0o600))
			case fixtureCancel:
				cancel()
			}

			status, decision, err := keepalive.Step(ctx, control == "signal")
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureOperatorStop, status.Reason)
			assert.Equal(t, fixtureNone, decision.Action)
		})
	}
}

func countedObservation(now *time.Time, calls *int) observerFunc {
	return func(context.Context, time.Time) (Observation, error) {
		*calls++

		return Observation{At: *now, Signals: nil}, nil
	}
}

func TestEnrollFailureClosesGate(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	save := keepalive.save
	keepalive.save = func(State, bool) error { return ErrStateInvalid }
	require.ErrorIs(t, keepalive.Enroll(), ErrStateInvalid)
	keepalive.save = save

	unarmedStep(t, keepalive)
	require.NoError(t, keepalive.Rearm())
	step(t, keepalive, fixtureNone, fixtureAdmit)
}

func TestStopDuringSave(t *testing.T) {
	t.Parallel()

	for _, name := range []string{fixtureStop, fixtureAliasStop} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			save := keepalive.save
			path := filepath.Join(keepalive.store.Dir, name)
			keepalive.save = func(state State, initial bool) error {
				require.NoError(t, save(state, initial))
				require.NoError(t, os.WriteFile(path, nil, privateFileMode))

				return nil
			}

			step(t, keepalive, fixtureOperatorStop, fixtureNone)
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Equal(t, uint64(1), state.InputCount)
			assert.Equal(t, []Attempt{{At: now, Debit: 0.1}}, state.Attempts)
			require.NoError(t, os.Remove(path))
			unarmedStep(t, keepalive)
		})
	}
}

func TestRollbackDuringObservation(t *testing.T) {
	t.Parallel()

	for _, call := range []string{"source", "observer"} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			reads := 0
			keepalive.source = sourceFunc(func(_ context.Context, at time.Time) (Snapshot, error) {
				reads++

				return snapshotAt(at), nil
			})

			// Keep the rollback later than the durable checkpoint.
			now = now.Add(time.Second)

			if call == "source" {
				keepalive.source = sourceFunc(func(_ context.Context, at time.Time) (Snapshot, error) {
					now = now.Add(-time.Nanosecond)

					return snapshotAt(at), nil
				})
			} else {
				keepalive.observer = observerFunc(func(_ context.Context, at time.Time) (Observation, error) {
					now = now.Add(-time.Nanosecond)

					return Observation{At: at, Signals: nil}, nil
				})
			}

			invalidStep(t, keepalive)

			if call == "observer" {
				assert.Zero(t, reads)
			}

			now = instant().Add(time.Second)

			unarmedStep(t, keepalive)
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Zero(t, state.InputCount)
			assert.Equal(t, fixtureStateInvalid, state.Status.Reason)

			keepalive.source = sourceFunc(readSnapshot)
			keepalive.observer = observationAt(now, nil)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}
}

func TestRollbackAfterSave(t *testing.T) {
	t.Parallel()

	for _, elapsed := range []time.Duration{0, time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			keepalive.source = sourceFunc(func(_ context.Context, at time.Time) (Snapshot, error) {
				now = now.Add(elapsed)

				return snapshotAt(at), nil
			})

			save := keepalive.save
			keepalive.save = func(state State, initial bool) error {
				keepalive.save = save
				require.NoError(t, save(state, initial))

				now = now.Add(-time.Nanosecond)

				return nil
			}

			status, decision, err := keepalive.Step(t.Context(), false)
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
			assert.Equal(t, phaseStopped, status.Phase)
			assert.Equal(t, fixtureNone, decision.Action)
			assert.True(t, decision.CheckAt.IsZero())
			assert.True(t, keepalive.invalidState)

			now = instant().Add(time.Second)

			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Equal(t, uint64(1), state.InputCount)
			assert.Equal(t, []Attempt{{At: instant(), Debit: 0.1}}, state.Attempts)
			assert.Equal(t, fixtureStateInvalid, state.Status.Reason)

			stored, err := keepalive.store.ReadStatus(fixtureWork)
			require.NoError(t, err)
			assert.Equal(t, status, stored)
			unarmedStep(t, keepalive)

			now = instant().Add(6 * time.Hour)

			unarmedStep(t, keepalive)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)
		})
	}
}

func TestRollbackBetweenAdapterSamples(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name    string
		samples []time.Time
	}{
		{"after Observe", []time.Time{instant(), instant().Add(-time.Nanosecond), instant()}},
		{"after Read", []time.Time{
			instant(), instant().Add(time.Second), instant().Add(time.Second - time.Nanosecond),
			instant().Add(time.Second),
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			samples := fixture.samples
			keepalive.now = func() time.Time {
				sampled := samples[0]
				samples = samples[1:]

				return sampled
			}

			status, decision, err := keepalive.Step(t.Context(), false)
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
			assert.Equal(t, phaseStopped, status.Phase)
			assert.Equal(t, fixtureNone, decision.Action)
			assert.True(t, decision.CheckAt.IsZero())
			assert.True(t, keepalive.invalidState)

			now = instant().Add(time.Second)
			keepalive.now = func() time.Time { return now }

			unarmedStep(t, keepalive)
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Zero(t, state.InputCount)
			assert.Empty(t, state.Attempts)
		})
	}
}

func TestBudgetLatchThroughSixHours(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		vendor   string
		interval time.Duration
	}{
		{fixtureClaude, 5 * time.Minute},
		{fixtureCodex, 15 * time.Minute},
	} {
		t.Run(fixture.vendor, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := fixtureKeepalive(t, &now)
			keepalive.enrollment.Vendor = fixture.vendor
			require.NoError(t, keepalive.Enroll())
			unarmedStep(t, keepalive)
			require.NoError(t, keepalive.Rearm())
			step(t, keepalive, fixtureNone, fixtureAdmit)

			now = instant().Add(time.Second)

			decision := step(t, keepalive, fixtureNone, fixtureWait)
			assert.Equal(t, now.Add(time.Second), decision.CheckAt)
			state, err := keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.True(t, state.PendingAt.IsZero())
			assert.Equal(t, now.Add(fixture.interval), state.DueAt)

			now = now.Add(fixture.interval)

			decision = step(t, keepalive, fixtureBudgetExhausted, fixtureNone)
			assert.True(t, decision.CheckAt.IsZero())

			stopped, err := keepalive.store.ReadStatus(fixtureWork)
			require.NoError(t, err)
			assert.Equal(t, phaseStopped, stopped.Phase)
			assert.True(t, keepalive.invalidState)

			for elapsed := fixture.interval + time.Minute; elapsed <= 6*time.Hour; elapsed += time.Minute {
				now = instant().Add(elapsed)

				unarmedStep(t, keepalive)
				state, err = keepalive.store.load(keepalive.enrollment, now)
				require.NoError(t, err)
				assert.Equal(t, uint64(1), state.InputCount)
				assert.Equal(t, []Attempt{{At: instant(), Debit: 0.1}}, state.Attempts)
				assert.Equal(t, stopped, state.Status)
			}

			require.NoError(t, keepalive.Rearm())
			state, err = keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Equal(t, stopped, state.Status)

			stored, err := keepalive.store.ReadStatus(fixtureWork)
			require.NoError(t, err)
			assert.Equal(t, stopped, stored)
			assert.Equal(t, uint64(1), state.InputCount)
			assert.Equal(t, []Attempt{{At: instant(), Debit: 0.1}}, state.Attempts)
			step(t, keepalive, fixtureNone, fixtureAdmit)
			state, err = keepalive.store.load(keepalive.enrollment, now)
			require.NoError(t, err)
			assert.Equal(t, uint64(2), state.InputCount)
			assert.Equal(t, []Attempt{{At: instant(), Debit: 0.1}, {At: now, Debit: 0.1}}, state.Attempts)
		})
	}
}

func TestSuccessfulRearmPreservesBothStops(t *testing.T) {
	t.Parallel()

	now := instant()
	keepalive := enrolledKeepalive(t, &now)
	_, _, err := keepalive.Step(t.Context(), true)
	require.NoError(t, err)
	before, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)

	status := before.Status
	status.Reason = fixtureStateInvalid

	require.NoError(t, keepalive.store.WriteStatus(status))

	now = now.Add(time.Second)

	require.NoError(t, keepalive.Rearm())

	after, err := keepalive.store.load(keepalive.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, before.Status, after.Status)

	storedStatus, err := keepalive.store.ReadStatus(fixtureWork)
	require.NoError(t, err)
	assert.Equal(t, status, storedStatus)
}

func readSnapshot(_ context.Context, at time.Time) (Snapshot, error) {
	return snapshotAt(at), nil
}

func TestSaveRecheckFailures(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		failure string
		reason  string
	}{
		{fixtureStopRead, fixtureStateInvalid},
		{"stop save", fixtureOperatorStop},
		{fixtureCancel, fixtureOperatorStop},
	} {
		t.Run(fixture.failure, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keepalive := enrolledKeepalive(t, &now)
			keepalive.source = sourceFunc(readSnapshot)
			keepalive.observer = observationAt(now, nil)
			ctx, cancel := context.WithCancel(t.Context())

			defer cancel()

			save := keepalive.save
			path := filepath.Join(keepalive.store.Dir, fixtureStop)
			keepalive.save = func(state State, initial bool) error {
				keepalive.save = save
				require.NoError(t, save(state, initial))

				switch fixture.failure {
				case fixtureStopRead:
					require.NoError(t, os.Mkdir(path, 0o700))
				case "stop save":
					require.NoError(t, os.WriteFile(path, nil, privateFileMode))

					keepalive.save = func(State, bool) error { return ErrStateInvalid }
				case fixtureCancel:
					cancel()
				}

				return nil
			}

			status, decision, err := keepalive.Step(ctx, false)
			if fixture.failure == fixtureCancel {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrStateInvalid)
			}

			assert.Equal(t, fixture.reason, status.Reason)
			assert.Equal(t, fixtureNone, decision.Action)

			keepalive.save = save

			if fixture.failure != fixtureCancel {
				require.NoError(t, os.Remove(path))
			}

			unarmedStep(t, keepalive)
		})
	}
}
