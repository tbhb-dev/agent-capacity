// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package keepalive records decisions and durable state without sending input.
// A restart or a new instance needs one explicit Rearm before it can admit.
// Operator stop files always win.
// The caller must check stop controls again at delivery.
// The keepalive logic is unproven against any harness.
package keepalive

import (
	"time"
)

const (
	processGone        = "process_gone"
	ownershipLost      = "ownership_lost"
	humanAttached      = "human_attached"
	readingUnavailable = "reading_unavailable"
	budgetExhausted    = "budget_exhausted"
	operatorStop       = "operator_stop"
	phaseRunning       = "running"
	phaseStuck         = "stuck"
	phaseStopped       = "stopped"
	reasonNone         = "none"
	vendorCodex        = "codex"
	vendorClaude       = "claude"
	vendorAGY          = "agy"
	refreshTimeout     = "refresh_timeout"
	readingInvalid     = "reading_invalid"
	unknownScreen      = "unknown_screen"
	unsupportedVersion = "unsupported_version"
	debitUnknown       = "debit_unknown"
	nearLimit          = "near_limit"
	stateInvalid       = "state_invalid"
	retryExhausted     = "retry_exhausted"
	claudeInterval     = 5 * time.Minute
	codexInterval      = 15 * time.Minute
	claudeFreshFor     = 10 * time.Minute
	codexFreshFor      = 30 * time.Minute
	usageLimit         = 0.85
	refreshWait        = 120 * time.Second
)

// notArmed means this instance needs an explicit successful Rearm.
const notArmed = "not_armed"

// Status contains only public labels and fixed codes.
type Status struct {
	At     time.Time
	Alias  string
	Vendor string
	Phase  string
	Reason string
}

// Stop maps classified signals to a phase and a fixed reason code.
// Callers classify observations without passing screen content to the core.
// The first classified stop takes precedence. An empty list means normal running.
func Stop(signals ...string) (string, string) {
	type stopCode struct{ phase, reason string }

	table := map[string]stopCode{
		"login_challenge":    {phaseStuck, "login_required"},
		"trust_prompt":       {phaseStuck, "trust_required"},
		"permission_prompt":  {phaseStuck, "permission_required"},
		"rate_limit":         {phaseStopped, "rate_limited"},
		"process_missing":    {phaseStuck, processGone},
		"pane_dead":          {phaseStuck, processGone},
		"pane_changed":       {phaseStuck, ownershipLost},
		"ownership_changed":  {phaseStuck, ownershipLost},
		"client_attached":    {phaseStopped, humanAttached},
		"unexpected_input":   {phaseStopped, humanAttached},
		refreshTimeout:       {phaseStuck, refreshTimeout},
		"reading_stale":      {phaseStopped, readingUnavailable},
		"reading_missing":    {phaseStopped, readingUnavailable},
		readingInvalid:       {phaseStopped, readingUnavailable},
		"unknown_dialog":     {phaseStuck, unknownScreen},
		"unknown_composer":   {phaseStuck, unknownScreen},
		unsupportedVersion:   {phaseStopped, unsupportedVersion},
		"count_cap":          {phaseStopped, budgetExhausted},
		"debit_allowance":    {phaseStopped, budgetExhausted},
		debitUnknown:         {phaseStopped, budgetExhausted},
		nearLimit:            {phaseStopped, nearLimit},
		"stop_file":          {phaseStopped, operatorStop},
		"stop_signal":        {phaseStopped, operatorStop},
		"ledger_lost":        {phaseStopped, stateInvalid},
		"clock_rollback":     {phaseStopped, stateInvalid},
		"future_observation": {phaseStopped, stateInvalid},
		retryExhausted:       {phaseStuck, retryExhausted},
	}

	for _, signal := range signals {
		if signal == "client_detached" {
			continue
		}

		code, found := table[signal]

		if !found {
			return phaseStuck, unknownScreen
		}

		return code.phase, code.reason
	}

	return phaseRunning, reasonNone
}

// Enrollment names the explicit windows and a lifetime debit allowance.
// An operator must supply a separate alias allow list too.
type Enrollment struct {
	Alias          string
	Vendor         string
	Windows        []string
	DebitAllowance float64
}

// Window is a classified quota observation, without harness content.
type Window struct {
	ObservedAt time.Time
	Name       string
	UsedRatio  float64
	Carried    bool
}

// Snapshot includes a conservative debit estimate. Nil means unknown debit.
type Snapshot struct {
	Debit   *float64
	Windows []Window
}

// Attempt counts even when a future delivery has an uncertain outcome.
type Attempt struct {
	At    time.Time
	Debit float64
}

// State is the durable ledger. No attempt expires from its audit history.
type State struct {
	LastAt     time.Time
	DueAt      time.Time
	PendingAt  time.Time
	Status     Status
	Attempts   []Attempt
	Enrollment Enrollment
	InputCount uint64
	Retry      int
}

// Decision describes one action and the next stop control check deadline.
// Admit records an attempt but never delivers anything.
type Decision struct {
	CheckAt time.Time
	Action  string
}

func interval(vendor string) time.Duration {
	if vendor == vendorCodex {
		return codexInterval
	}

	return claudeInterval
}

// Next skips missed ticks. A caller must check stop controls by CheckAt,
// even while the next quota observation remains minutes away.
func Next(state State, now time.Time) Decision {
	if state.Status.Phase != phaseRunning {
		return Decision{Action: reasonNone, CheckAt: time.Time{}}
	}

	if !now.Before(state.DueAt) {
		return Decision{Action: "observe", CheckAt: now}
	}

	return Decision{Action: "wait", CheckAt: minTime(now.Add(time.Second), state.DueAt)}
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}

	return second
}

func readingSignal(enrollment Enrollment, snapshot Snapshot, now time.Time) string {
	if len(snapshot.Windows) != len(enrollment.Windows) {
		return "reading_missing"
	}

	for _, name := range enrollment.Windows {
		var matches []Window

		for _, window := range snapshot.Windows {
			if window.Name == name {
				matches = append(matches, window)
			}
		}

		if len(matches) != 1 {
			return readingInvalid
		}

		if signal := windowSignal(matches[0], enrollment.Vendor, now); signal != "" {
			return signal
		}
	}

	return ""
}

func windowSignal(window Window, vendor string, now time.Time) string {
	if window.ObservedAt.After(now) {
		return "future_observation"
	}

	if window.ObservedAt.IsZero() || window.Carried || !ratio(window.UsedRatio) {
		return readingInvalid
	}

	freshFor := claudeFreshFor

	if vendor == vendorCodex {
		freshFor = codexFreshFor
	}

	if now.Sub(window.ObservedAt) >= freshFor {
		return "reading_stale"
	}

	if window.UsedRatio >= usageLimit {
		return nearLimit
	}

	return ""
}

func ratio(value float64) bool {
	return value >= 0 && value <= 1
}

func budgetSignal(state State, debit *float64, now time.Time) string {
	if debit == nil {
		return debitUnknown
	}

	if !ratio(*debit) {
		return debitUnknown
	}

	var recent, weekly int

	total := *debit

	for _, attempt := range state.Attempts {
		total += attempt.Debit

		age := now.Sub(attempt.At)

		if age < 5*time.Hour {
			recent++
		}

		if age < 7*24*time.Hour {
			weekly++
		}
	}

	if recent >= 1 || weekly >= 2 {
		return "count_cap"
	}

	if total > state.Enrollment.DebitAllowance {
		return "debit_allowance"
	}

	return ""
}

func latch(state *State, signal string, now time.Time) {
	state.Status.Phase, state.Status.Reason = Stop(signal)
	state.Status.At = now
	state.DueAt = time.Time{}
}

// evaluate is the functional admission core. It never clears a latched stop.
// Confirming a refresh requires new observations for every enrolled window.
func evaluate(state State, now time.Time, signals []string, snapshot Snapshot) (State, Decision) {
	if !validState(state, state.Enrollment, now) {
		latch(&state, "clock_rollback", now)

		return state, Next(state, now)
	}

	if state.Status.Phase != phaseRunning {
		return state, Next(state, now)
	}

	state.LastAt = now

	if !state.PendingAt.IsZero() && now.Sub(state.PendingAt) >= refreshWait {
		latch(&state, refreshTimeout, now)

		return state, Next(state, now)
	}

	phase, reason := Stop(signals...)

	if phase != phaseRunning {
		state.Status.Phase, state.Status.Reason, state.Status.At = phase, reason, now
		state.DueAt = time.Time{}

		return state, Next(state, now)
	}

	if signal := readingSignal(state.Enrollment, snapshot, now); signal != "" {
		latch(&state, signal, now)

		return state, Next(state, now)
	}

	state.DueAt = now.Add(interval(state.Enrollment.Vendor))

	if !state.PendingAt.IsZero() {
		return confirm(state, snapshot, now)
	}

	// A successful observation retry never admits an input.
	if state.Retry > 0 {
		state.Retry = 0

		return state, Next(state, now)
	}

	if signal := budgetSignal(state, snapshot.Debit, now); signal != "" {
		// Budget exhaustion persists until an explicit Rearm.
		latch(&state, signal, now)

		return state, Next(state, now)
	}

	state.Attempts = append(append([]Attempt(nil), state.Attempts...), Attempt{At: now, Debit: *snapshot.Debit})
	state.InputCount++
	state.PendingAt = now
	state.DueAt = now.Add(time.Second)

	return state, Decision{Action: "admit", CheckAt: state.DueAt}
}

func confirm(state State, snapshot Snapshot, now time.Time) (State, Decision) {
	for _, window := range snapshot.Windows {
		if !window.ObservedAt.After(state.PendingAt) {
			state.DueAt = now.Add(time.Second)

			return state, Next(state, now)
		}
	}

	state.PendingAt = time.Time{}
	state.Retry = 0

	return state, Next(state, now)
}

// retry schedules observation only. The third failed retry latches a stop.
func retry(state State, now time.Time) State {
	delays := [...]time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

	if state.Retry == len(delays) {
		latch(&state, retryExhausted, now)

		return state
	}

	state.DueAt = now.Add(delays[state.Retry])
	state.Retry++

	return state
}
