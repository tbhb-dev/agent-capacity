// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//nolint:testpackage // Fault fixtures exercise internal filesystem errors and pure guards.
package keeper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func fixtureKeeper(t *testing.T, now *time.Time) *Keeper {
	t.Helper()

	keeper, err := New(Store{Dir: spoolDir(t)}, enrollment(), []string{fixtureWork}, func() time.Time { return *now },
		sourceFunc(func(ctx context.Context, at time.Time) (Snapshot, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, *now, at)

			return snapshotAt(at), nil
		}), observerFunc(func(ctx context.Context, at time.Time) (Observation, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, *now, at)

			return Observation{At: at, Signals: nil}, nil
		}))
	require.NoError(t, err)

	return keeper
}

func enrolledKeeper(t *testing.T, now *time.Time) *Keeper {
	t.Helper()

	keeper := fixtureKeeper(t, now)
	require.NoError(t, keeper.Enroll())

	return keeper
}

func step(t *testing.T, keeper *Keeper, wantReason, wantAction string) Decision {
	t.Helper()

	status, decision, err := keeper.Step(t.Context(), false)
	require.NoError(t, err)
	assert.Equal(t, wantReason, status.Reason)
	assert.Equal(t, wantAction, decision.Action)

	stored, err := keeper.store.ReadStatus(keeper.enrollment.Alias)
	require.NoError(t, err)
	assert.Equal(t, status, stored)

	return decision
}

func TestRestartAndRearm(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := fixtureKeeper(t, &now)
	require.NoError(t, keeper.Enroll())
	require.ErrorIs(t, keeper.Enroll(), ErrStateInvalid)
	step(t, keeper, fixtureNone, fixtureAdmit)

	state, err := keeper.store.load(keeper.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), state.InputCount)
	assert.Equal(t, []Attempt{{At: now, Debit: 0.1}}, state.Attempts)

	// A new instance reads the pending attempt instead of repeating it.
	restarted, err := New(keeper.store, enrollment(), []string{fixtureWork}, keeper.now, keeper.source, keeper.observer)
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
			keeper := fixtureKeeper(t, &now)
			require.NoError(t, keeper.Enroll())

			state, err := keeper.store.load(keeper.enrollment, now)
			require.NoError(t, err)

			state.DueAt = now.Add(time.Hour)
			require.NoError(t, keeper.store.save(state, false))

			decision := step(t, keeper, fixtureNone, fixtureWait)
			assert.Equal(t, now.Add(time.Second), decision.CheckAt)
			require.NoError(t, os.WriteFile(filepath.Join(keeper.store.Dir, name), nil, 0o600))

			now = decision.CheckAt
			decision = step(t, keeper, fixtureOperatorStop, fixtureNone)
			assert.True(t, decision.CheckAt.IsZero())
			require.ErrorIs(t, keeper.Rearm(), ErrStateInvalid)
			require.NoError(t, os.Remove(filepath.Join(keeper.store.Dir, name)))
			step(t, keeper, fixtureOperatorStop, fixtureNone)
			require.NoError(t, keeper.Rearm())
			step(t, keeper, fixtureNone, fixtureAdmit)
		})
	}

	for _, cancelContext := range []bool{false, true} {
		now := instant()
		keeper := fixtureKeeper(t, &now)
		require.NoError(t, keeper.Enroll())

		ctx, cancel := context.WithCancel(t.Context())

		if cancelContext {
			cancel()
		}

		status, decision, err := keeper.Step(ctx, !cancelContext)

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
			keeper := enrolledKeeper(t, &now)

			ctx, cancel := context.WithCancel(t.Context())

			defer cancel()

			keeper.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
				switch action {
				case fixtureStop:
					require.NoError(t, os.WriteFile(filepath.Join(keeper.store.Dir, fixtureStop), nil, 0o600))
				case fixtureCancel:
					cancel()
				case "lose ledger":
					require.NoError(t, os.Remove(filepath.Join(keeper.store.Dir, "work.ledger")))
				case "bad stop":
					require.NoError(t, os.Mkdir(filepath.Join(keeper.store.Dir, fixtureStop), 0o700))
				case "bad status":
					require.NoError(t, os.Remove(filepath.Join(keeper.store.Dir, "work.status")))
					require.NoError(t, os.Mkdir(filepath.Join(keeper.store.Dir, "work.status"), 0o700))
				case "bad directory":
					require.NoError(t, os.RemoveAll(keeper.store.Dir))
				}

				return snapshotAt(now), nil
			})
			keeper.observer = observationAt(now, nil)

			status, decision, err := keeper.Step(ctx, false)

			if action == fixtureStop || action == fixtureCancel {
				require.NoError(t, err)
				assert.Equal(t, fixtureOperatorStop, status.Reason)

				state, loadErr := keeper.store.load(keeper.enrollment, now)
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
			keeper := enrolledKeeper(t, &now)

			keeper.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
				return fixture.observation, nil
			})
			keeper.source = sourceFunc(func(context.Context, time.Time) (Snapshot, error) {
				return snapshotAt(now), fixture.sourceError
			})

			wantAction := fixtureNone

			if fixture.want == fixtureNone {
				wantAction = fixtureAdmit
			}

			step(t, keeper, fixture.want, wantAction)
		})
	}
}

func TestPersistedRetries(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := enrolledKeeper(t, &now)

	keeper.observer = observationAt(time.Time{}, errors.ErrUnsupported)

	for index, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		step(t, keeper, fixtureNone, fixtureWait)

		state, err := keeper.store.load(keeper.enrollment, now)
		require.NoError(t, err)
		assert.Equal(t, now.Add(delay), state.DueAt)
		assert.Equal(t, index+1, state.Retry)
		assert.Zero(t, state.InputCount)

		restarted, newErr := New(
			keeper.store,
			enrollment(),
			[]string{fixtureWork},
			keeper.now,
			keeper.source,
			keeper.observer,
		)
		require.NoError(t, newErr)

		keeper = restarted
		now = state.DueAt.Add(-time.Nanosecond)

		step(t, keeper, fixtureNone, fixtureWait)

		now = state.DueAt
	}

	step(t, keeper, "retry_exhausted", fixtureNone)
	require.NoError(t, keeper.Rearm())

	keeper.observer = observationAt(now, nil)
	step(t, keeper, fixtureNone, fixtureAdmit)
}

func TestPendingTimeoutAndStateLoss(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"timeout", "rollback", fixtureMissing, "corrupt", "stop directory"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			now := instant()
			keeper := fixtureKeeper(t, &now)
			require.NoError(t, keeper.Enroll())
			step(t, keeper, fixtureNone, fixtureAdmit)

			switch action {
			case "timeout":
				now = now.Add(120 * time.Second)

				step(t, keeper, fixtureRefreshTimeout, fixtureNone)

				return
			case "rollback":
				now = now.Add(-time.Nanosecond)
			case fixtureMissing:
				require.NoError(t, os.Remove(filepath.Join(keeper.store.Dir, "work.ledger")))
			case "corrupt":
				require.NoError(t, os.WriteFile(filepath.Join(keeper.store.Dir, "work.ledger"), []byte("{"), 0o600))
			case "stop directory":
				require.NoError(t, os.Mkdir(filepath.Join(keeper.store.Dir, "work.stop"), 0o700))
			}

			status, decision, err := keeper.Step(t.Context(), false)
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
			assert.Equal(t, "stopped", status.Phase)
			assert.Equal(t, fixtureNone, decision.Action)

			stored, statusErr := keeper.store.ReadStatus(fixtureWork)
			require.NoError(t, statusErr)
			assert.Equal(t, fixtureStateInvalid, stored.Reason)
			require.ErrorIs(t, keeper.Rearm(), ErrStateInvalid)
		})
	}
}

func TestEnrollment(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := fixtureKeeper(t, &now)
	copyEnrollment := enrollment()
	created, err := New(keeper.store, copyEnrollment, []string{fixtureWork}, keeper.now, keeper.source, keeper.observer)
	require.NoError(t, err)

	copyEnrollment.Windows[0] = fixtureOther
	assert.Equal(t, "5h", created.enrollment.Windows[0])

	for _, fixture := range []struct {
		clock    func() time.Time
		source   Source
		observer Observer
		allow    []string
	}{
		{keeper.now, keeper.source, keeper.observer, []string{fixturePersonal}},
		{keeper.now, keeper.source, keeper.observer, nil},
		{nil, keeper.source, keeper.observer, []string{fixtureWork}},
		{keeper.now, nil, keeper.observer, []string{fixtureWork}},
		{keeper.now, keeper.source, nil, []string{fixtureWork}},
	} {
		created, err = New(keeper.store, enrollment(), fixture.allow, fixture.clock, fixture.source, fixture.observer)
		require.ErrorIs(t, err, ErrStateInvalid)
		assert.Nil(t, created)
	}

	status, decision, err := keeper.Step(t.Context(), false)
	require.ErrorIs(t, err, ErrStateInvalid)
	assert.Equal(t, fixtureStateInvalid, status.Reason)
	assert.Equal(t, fixtureNone, decision.Action)

	now = time.Time{}

	require.ErrorIs(t, keeper.Enroll(), ErrStateInvalid)
}

func TestOperatorLatchSurvivesTimeout(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := fixtureKeeper(t, &now)
	require.NoError(t, keeper.Enroll())
	step(t, keeper, fixtureNone, fixtureAdmit)

	status, decision, err := keeper.Step(t.Context(), true)
	require.NoError(t, err)
	assert.Equal(t, fixtureOperatorStop, status.Reason)
	assert.Equal(t, fixtureNone, decision.Action)

	now = now.Add(120 * time.Second)

	step(t, keeper, fixtureOperatorStop, fixtureNone)
	require.NoError(t, keeper.Rearm())
	step(t, keeper, fixtureBudgetExhausted, fixtureNone)
}

func TestEnrollmentCannotReplaceLostLedger(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := fixtureKeeper(t, &now)
	require.NoError(t, keeper.Enroll())
	step(t, keeper, fixtureNone, fixtureAdmit)
	require.NoError(t, os.Remove(filepath.Join(keeper.store.Dir, "work.ledger")))
	require.ErrorIs(t, keeper.Enroll(), ErrStateInvalid)

	_, err := keeper.store.load(keeper.enrollment, now)
	require.ErrorIs(t, err, ErrStateInvalid)
}

func TestStopPreventsObservation(t *testing.T) {
	t.Parallel()

	for _, invalid := range []bool{false, true} {
		now := instant()
		keeper := fixtureKeeper(t, &now)
		require.NoError(t, keeper.Enroll())

		name := filepath.Join(keeper.store.Dir, fixtureStop)

		if invalid {
			require.NoError(t, os.Mkdir(name, 0o700))
		} else {
			require.NoError(t, os.WriteFile(name, nil, 0o600))
		}

		observed := 0

		keeper.observer = observerFunc(func(context.Context, time.Time) (Observation, error) {
			observed++

			return Observation{At: now, Signals: nil}, nil
		})
		status, decision, err := keeper.Step(t.Context(), false)

		if invalid {
			require.ErrorIs(t, err, ErrStateInvalid)
			assert.Equal(t, fixtureStateInvalid, status.Reason)
		} else {
			require.NoError(t, err)
			assert.Equal(t, fixtureOperatorStop, status.Reason)
		}

		assert.Equal(t, fixtureNone, decision.Action)
		assert.Zero(t, observed)

		state, loadErr := keeper.store.load(keeper.enrollment, now)
		require.NoError(t, loadErr)
		assert.Zero(t, state.InputCount)
	}
}

func TestTimeoutDuringRetryWait(t *testing.T) {
	t.Parallel()

	now := instant()
	keeper := enrolledKeeper(t, &now)
	step(t, keeper, fixtureNone, fixtureAdmit)

	keeper.observer = observationAt(time.Time{}, errors.ErrUnsupported)

	now = instant().Add(time.Second)

	step(t, keeper, fixtureNone, fixtureWait)

	now = now.Add(30 * time.Second)

	step(t, keeper, fixtureNone, fixtureWait)

	state, err := keeper.store.load(keeper.enrollment, now)
	require.NoError(t, err)
	assert.Equal(t, instant().Add(151*time.Second), state.DueAt)

	now = instant().Add(120 * time.Second)

	step(t, keeper, fixtureRefreshTimeout, fixtureNone)
}

func TestZeroClockAndObservation(t *testing.T) {
	t.Parallel()

	now := time.Time{}
	keeper := fixtureKeeper(t, &now)
	require.ErrorIs(t, keeper.Enroll(), ErrStateInvalid)

	entries, err := os.ReadDir(keeper.store.Dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	now = now.Add(time.Second)

	require.NoError(t, keeper.Enroll())

	keeper.observer = observationAt(time.Time{}, nil)

	step(t, keeper, fixtureUnknownScreen, fixtureNone)
}
