// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package keeper

import (
	"context"
	"errors"
	"time"
)

// Source supplies classified quota windows without reading harness state here.
type Source interface {
	Read(ctx context.Context, now time.Time) (Snapshot, error)
}

// Observation contains only classified signals and their observation time.
type Observation struct {
	At      time.Time
	Signals []string
}

// Observer abstracts future pane observation. This package has no adapter.
type Observer interface {
	Observe(ctx context.Context, now time.Time) (Observation, error)
}

// Keeper coordinates an allowed alias. The owner serializes calls and meets CheckAt deadlines.
type Keeper struct {
	now        func() time.Time
	source     Source
	observer   Observer
	store      Store
	enrollment Enrollment
}

// New validates explicit enrollment. It never creates or replaces a ledger.
func New(store Store, enrollment Enrollment, allow []string, now func() time.Time,
	source Source, observer Observer,
) (*Keeper, error) {
	if !validEnrollment(enrollment, allow) || now == nil || source == nil || observer == nil {
		return nil, ErrStateInvalid
	}

	enrollment.Windows = append([]string(nil), enrollment.Windows...)

	return &Keeper{store: store, enrollment: enrollment, now: now, source: source, observer: observer}, nil
}

// Enroll creates a ledger once, as an explicit operator action. It refuses to
// overwrite an existing ledger, including an unreadable or uncertain one.
// Use an absolute spool path under exclusive ownership. If the status record
// remains after ledger loss, restore the ledger's history before using the core.
func (keeper *Keeper) Enroll() error {
	now := keeper.now()

	state := State{
		Status: Status{
			Alias: keeper.enrollment.Alias, Vendor: keeper.enrollment.Vendor,
			Phase: phaseRunning, Reason: reasonNone, At: now,
		},
		Enrollment: keeper.enrollment, Attempts: nil, InputCount: 0,
		LastAt: now, DueAt: now, PendingAt: time.Time{}, Retry: 0,
	}

	return keeper.store.save(state, true)
}

// Rearm explicitly clears a latch while preserving counts and debits.
// It can't repair a lost ledger or bypass a stop file.
func (keeper *Keeper) Rearm() error {
	now := keeper.now()

	state, err := keeper.store.load(keeper.enrollment, now)
	// gomutants:disable-next-line BRANCH_IF reason="Failed loads return empty state that also fails save."
	if err != nil {
		return ErrStateInvalid
	}

	stopped, err := keeper.store.stopped(keeper.enrollment.Alias)
	if err != nil || stopped {
		return ErrStateInvalid
	}

	state.Status.Phase, state.Status.Reason, state.Status.At = phaseRunning, reasonNone, now
	state.LastAt, state.DueAt = now, now
	state.PendingAt, state.Retry = time.Time{}, 0

	return keeper.store.save(state, false)
}

// Step checks stops before admission and persists attempts before returning.
// Call at least once per second while running. Errors suppress admission.
func (keeper *Keeper) Step(ctx context.Context, stopSignal bool) (Status, Decision, error) {
	now := keeper.now()

	state, err := keeper.store.load(keeper.enrollment, now)
	// gomutants:disable-next-line BRANCH_IF reason="Failed loads return empty state that also fails save."
	if err != nil {
		return keeper.invalid(now)
	}

	stopped, err := keeper.store.stopped(keeper.enrollment.Alias)
	if err != nil {
		return keeper.invalid(now)
	}

	if stopped || stopSignal || ctx.Err() != nil {
		latch(&state, "stop_signal", now)
	}

	state.LastAt = now

	if !state.PendingAt.IsZero() && now.Sub(state.PendingAt) >= refreshWait && state.Status.Phase == phaseRunning {
		latch(&state, refreshTimeout, now)
	}

	decision := Next(state, now)

	if decision.Action == "observe" {
		state, decision = keeper.observe(ctx, state, now)
	}

	stopped, err = keeper.store.stopped(keeper.enrollment.Alias)
	if err != nil {
		return keeper.invalid(now)
	}

	if stopped || ctx.Err() != nil {
		latch(&state, "stop_signal", now)
		decision = Next(state, now)
	}

	if keeper.store.save(state, false) != nil {
		return keeper.invalid(now)
	}

	return state.Status, decision, nil
}

func (keeper *Keeper) observe(ctx context.Context, state State, now time.Time) (State, Decision) {
	observation, err := keeper.observer.Observe(ctx, now)
	if err != nil {
		state = retry(state, now)

		return state, Next(state, now)
	}

	if observation.At.After(now) {
		latch(&state, "future_observation", now)

		return state, Next(state, now)
	}

	if observation.At.IsZero() || now.Sub(observation.At) > time.Second {
		latch(&state, "unknown_dialog", now)

		return state, Next(state, now)
	}

	phase, _ := Stop(observation.Signals...)

	if phase != phaseRunning {
		return evaluate(state, now, observation.Signals, Snapshot{Windows: nil, Debit: nil})
	}

	snapshot, err := keeper.source.Read(ctx, now)
	if err != nil {
		latch(&state, readingInvalid, now)

		return state, Next(state, now)
	}

	return evaluate(state, now, nil, snapshot)
}

func (keeper *Keeper) invalid(now time.Time) (Status, Decision, error) {
	status := Status{
		Alias: keeper.enrollment.Alias, Vendor: keeper.enrollment.Vendor,
		Phase: phaseStopped, Reason: stateInvalid, At: now,
	}

	return status, Decision{
		Action:  reasonNone,
		CheckAt: time.Time{},
	}, errors.Join(
		ErrStateInvalid,
		keeper.store.WriteStatus(status),
	)
}
