// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package keepalive

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

// Keepalive coordinates an allowed alias. The owner serializes calls and meets CheckAt deadlines.
type Keepalive struct {
	now          func() time.Time
	source       Source
	observer     Observer
	store        Store
	save         func(State, bool) error
	enrollment   Enrollment
	invalidState bool
}

// New validates explicit enrollment. It never creates or replaces a ledger.
func New(store Store, enrollment Enrollment, allow []string, now func() time.Time,
	source Source, observer Observer,
) (*Keepalive, error) {
	if !validEnrollment(enrollment, allow) || now == nil || source == nil || observer == nil {
		return nil, ErrStateInvalid
	}

	enrollment.Windows = append([]string(nil), enrollment.Windows...)

	return &Keepalive{
		store:        store,
		save:         store.save,
		enrollment:   enrollment,
		now:          now,
		source:       source,
		observer:     observer,
		invalidState: true,
	}, nil
}

// Enroll creates a ledger once, as an explicit operator action. It refuses to
// overwrite an existing ledger, including an unreadable or uncertain one.
// Use an absolute spool path under exclusive ownership. If the status record
// remains after ledger loss, restore the ledger's history before using the core.
func (keepalive *Keepalive) Enroll() error {
	keepalive.invalidState = true

	now := keepalive.sample()

	state := State{
		Status: Status{
			Alias: keepalive.enrollment.Alias, Vendor: keepalive.enrollment.Vendor,
			Phase: phaseRunning, Reason: reasonNone, At: now,
		},
		Enrollment: keepalive.enrollment, Attempts: nil, InputCount: 0,
		LastAt: now, DueAt: now, PendingAt: time.Time{}, Retry: 0,
	}

	return keepalive.save(state, true)
}

// Rearm explicitly clears a latch while preserving counts and debits.
// It can't repair a lost ledger or bypass a stop file.
// Durable stops stay in place until the next armed Step succeeds.
func (keepalive *Keepalive) Rearm() error {
	keepalive.invalidState = true
	now := keepalive.sample()

	state, err := keepalive.store.load(keepalive.enrollment, now)
	// gomutants:disable-next-line BRANCH_IF reason="Failed loads return empty state that also fails save."
	if err != nil {
		return ErrStateInvalid
	}

	stopped, err := keepalive.store.stopped(keepalive.enrollment.Alias)
	if err != nil || stopped {
		return ErrStateInvalid
	}

	state.LastAt, state.DueAt = now, now
	state.PendingAt, state.Retry = time.Time{}, 0

	if keepalive.store.rearm(state) != nil {
		return ErrStateInvalid
	}

	last := keepalive.sample()
	if !validState(state, keepalive.enrollment, last) {
		return ErrStateInvalid
	}

	// Persist the sample from the write recheck.
	state.LastAt = last

	if keepalive.store.rearm(state) != nil {
		return ErrStateInvalid
	}

	stopped, err = keepalive.store.stopped(keepalive.enrollment.Alias)
	if err != nil || stopped {
		return ErrStateInvalid
	}

	keepalive.invalidState = false

	return nil
}

// Step rechecks stops after saving and before it reports admit.
// It persists attempts without sending input.
// A restart or a new instance needs one explicit Rearm before it can admit.
// Operator stop files always win. Errors require another successful Rearm.
// Call at least once per second while running.
// The caller must check stop controls again at delivery.
func (keepalive *Keepalive) Step(ctx context.Context, stopSignal bool) (Status, Decision, error) {
	now := keepalive.sample()

	if keepalive.invalidState {
		return keepalive.blocked(ctx, stopSignal, now)
	}

	state, err := keepalive.store.load(keepalive.enrollment, now)
	// gomutants:disable-next-line BRANCH_IF reason="Failed loads return empty state that also fails save."
	if err != nil {
		return keepalive.invalid(ctx, stopSignal, now, nil)
	}

	stopped, err := keepalive.store.stopped(keepalive.enrollment.Alias)
	if err != nil {
		return keepalive.invalid(ctx, stopSignal, now, &state)
	}

	if stopped || stopSignal || ctx.Err() != nil {
		latch(&state, "stop_signal", now)
	} else {
		// Rearm preserves durable stops until this armed Step can replace them.
		state.Status.Phase, state.Status.Reason, state.Status.At = phaseRunning, reasonNone, now
	}

	state.LastAt = now

	if !state.PendingAt.IsZero() && now.Sub(state.PendingAt) >= refreshWait && state.Status.Phase == phaseRunning {
		latch(&state, refreshTimeout, now)
	}

	decision := Next(state, now)
	last := now

	if decision.Action == "observe" {
		state, decision, err = keepalive.observe(ctx, state, now, &last)
		if err != nil {
			state.LastAt = last

			return keepalive.invalid(ctx, stopSignal, now, &state)
		}
	}

	keepalive.rememberInvalid(state.Status)

	return keepalive.savedStep(ctx, stopSignal, now, last, state, decision)
}

func (keepalive *Keepalive) sample() time.Time {
	// gomutants:disable-next-line INTEGER_INCREMENT,INTEGER_DECREMENT reason="Both mutants leave wall time unchanged."
	return keepalive.now().Round(0)
}

func (keepalive *Keepalive) savedStep(
	ctx context.Context, stopSignal bool, now, last time.Time, state State, decision Decision,
) (Status, Decision, error) {
	state.LastAt = last

	if keepalive.save(state, false) != nil {
		return keepalive.invalid(ctx, stopSignal, now, &state)
	}

	sampled := keepalive.sample()
	if sampled.Before(last) {
		return keepalive.invalid(ctx, stopSignal, now, &state)
	}

	// Persist the sample from the write recheck.
	state.LastAt = sampled

	if keepalive.save(state, false) != nil {
		return keepalive.invalid(ctx, stopSignal, now, &state)
	}

	stopped, err := keepalive.store.stopped(keepalive.enrollment.Alias)
	if err != nil {
		return keepalive.invalid(ctx, stopSignal, now, &state)
	}

	if stopped || ctx.Err() != nil {
		keepalive.invalidState = true

		latch(&state, "stop_signal", now)
		decision = Next(state, now)

		if keepalive.save(state, false) != nil {
			return keepalive.invalid(ctx, stopSignal, now, &state)
		}
	}

	return state.Status, decision, nil
}

func (keepalive *Keepalive) blocked(ctx context.Context, stopSignal bool, now time.Time) (Status, Decision, error) {
	stopped, err := keepalive.store.stopped(keepalive.enrollment.Alias)
	if err != nil {
		return keepalive.invalid(ctx, stopSignal, now, nil)
	}

	status := Status{
		Alias: keepalive.enrollment.Alias, Vendor: keepalive.enrollment.Vendor,
		Phase: phaseStopped, Reason: notArmed, At: now,
	}
	if stopped || stopSignal || ctx.Err() != nil {
		status.Reason = operatorStop
	}

	return status, Decision{Action: reasonNone, CheckAt: time.Time{}}, nil
}

func (keepalive *Keepalive) observe(
	ctx context.Context, state State, now time.Time, last *time.Time,
) (State, Decision, error) {
	observation, err := keepalive.observer.Observe(ctx, now)
	sampled := keepalive.sample()

	if sampled.Before(*last) {
		return state, Decision{}, ErrStateInvalid
	}

	*last = sampled

	if err != nil {
		keepalive.invalidState = true
		state = retry(state, now)

		return state, Next(state, now), nil //nolint:nilerr // Retry decisions handle errors. cspell:ignore nilerr
	}

	if observation.At.After(now) {
		latch(&state, "future_observation", now)

		return state, Next(state, now), nil
	}

	if observation.At.IsZero() || now.Sub(observation.At) > time.Second {
		latch(&state, "unknown_dialog", now)

		return state, Next(state, now), nil
	}

	phase, _ := Stop(observation.Signals...)

	if phase != phaseRunning {
		var decision Decision

		state, decision = evaluate(state, now, observation.Signals, Snapshot{Windows: nil, Debit: nil})

		return state, decision, nil
	}

	snapshot, err := keepalive.source.Read(ctx, now)
	sampled = keepalive.sample()

	if sampled.Before(*last) {
		return state, Decision{}, ErrStateInvalid
	}

	*last = sampled

	if err != nil {
		latch(&state, readingInvalid, now)

		return state, Next(state, now), nil //nolint:nilerr // Reading errors become stop codes. cspell:ignore nilerr
	}

	state, decision := evaluate(state, now, nil, snapshot)

	return state, decision, nil
}

func (keepalive *Keepalive) rememberInvalid(status Status) {
	if status.Phase != phaseRunning {
		keepalive.invalidState = true
	}
}

func (keepalive *Keepalive) invalid(
	ctx context.Context, stopSignal bool, now time.Time, state *State,
) (Status, Decision, error) {
	keepalive.invalidState = true

	status := Status{
		Alias: keepalive.enrollment.Alias, Vendor: keepalive.enrollment.Vendor,
		Phase: phaseStopped, Reason: stateInvalid, At: now,
	}

	stopped, stopErr := keepalive.store.stopped(keepalive.enrollment.Alias)
	if stopped || stopSignal || ctx.Err() != nil {
		status.Reason = operatorStop
	}

	return status, Decision{
		Action:  reasonNone,
		CheckAt: time.Time{},
	}, errors.Join(
		ErrStateInvalid,
		stopErr,
		keepalive.saveInvalid(state, now),
		keepalive.store.WriteStatus(status),
	)
}

func (keepalive *Keepalive) saveInvalid(state *State, now time.Time) error {
	if state == nil {
		return nil
	}

	latch(state, "ledger_lost", now)

	if now.After(state.LastAt) {
		state.LastAt = now
	}

	return keepalive.save(*state, false)
}
