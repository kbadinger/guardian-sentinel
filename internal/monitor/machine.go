// Package monitor turns check results into alerts.
//
// Machine is the state machine: probe results in, alerts out, no I/O and no
// clock of its own (the caller passes the time), so every rule is tested
// with a plain table. Runner (runner.go) is the scheduler that runs the
// checks and sends what Machine returns.
package monitor

import (
	"fmt"
	"strings"
	"time"

	"github.com/kbadinger/guardian-sentinel/internal/probe"
)

// Kind says what an alert is about.
type Kind string

const (
	KindDown     Kind = "down"     // a check crossed its failure threshold, or is still down after the cooldown
	KindRecovery Kind = "recovery" // a down check passed again
	KindWarn     Kind = "warn"     // e.g. a cert about to expire; never marks a check down
	KindInfo     Kind = "info"     // e.g. a peer rebooted
	KindSummary  Kind = "summary"  // the startup "online — N/N ✓" message
)

// Alert is one message to send.
type Alert struct {
	Kind  Kind
	Check string // the check it is about; empty for the summary
	Text  string // the full plain-text message
}

// Settings configure a Machine.
type Settings struct {
	Instance      string         // shown as sentinel[<instance>]
	FailThreshold int            // default consecutive failures before DOWN
	Cooldown      time.Duration  // realert_cooldown: gap between repeat DOWN alerts, and between WARNs
	Location      *time.Location // zone for alert timestamps (America/New_York, shown as "ET")
}

// Machine tracks every check's state. It is not safe for concurrent use;
// Runner serializes access.
type Machine struct {
	s      Settings
	checks map[string]*checkState
}

type checkState struct {
	threshold int  // consecutive failures before DOWN
	peer      bool // a heartbeat peer: DOWN reads "<name> is down"

	failures     int       // consecutive failures so far
	firstFailure time.Time // when the current run of failures began
	lastErr      string
	down         bool
	lastDown     time.Time // when the last DOWN alert went out
	lastWarn     time.Time // when the last WARN went out; zero means never
	lastAt       time.Time // time of the newest result applied
}

// NewMachine returns a Machine with no checks tracked yet.
func NewMachine(s Settings) *Machine {
	if s.FailThreshold < 1 {
		s.FailThreshold = 1
	}
	if s.Location == nil {
		s.Location = time.UTC
	}
	return &Machine{s: s, checks: map[string]*checkState{}}
}

// Track registers a check. threshold <= 0 means the default fail threshold.
// peer marks a heartbeat peer, whose silence is already measured by
// max_silence, so callers give it threshold 1.
func (m *Machine) Track(name string, threshold int, peer bool) {
	if threshold <= 0 {
		threshold = m.s.FailThreshold
	}
	m.checks[name] = &checkState{threshold: threshold, peer: peer}
}

// Observe applies one result, measured at time at, and returns the alerts
// it causes. A result older than one already applied for the same check is
// ignored: a heartbeat can arrive while a slower round is still running,
// and the stale "silent" reading from that round must not undo it.
func (m *Machine) Observe(name string, r probe.Result, at time.Time) []Alert {
	s, ok := m.checks[name]
	if !ok {
		m.Track(name, 0, false)
		s = m.checks[name]
	}
	if at.Before(s.lastAt) {
		return nil
	}
	s.lastAt = at

	var out []Alert
	if r.OK {
		if s.down {
			out = append(out, m.alert(KindRecovery, name, "%s RECOVERED after %s", name, probe.FormatDuration(at.Sub(s.firstFailure))))
		}
		s.failures, s.down = 0, false
	} else {
		if s.failures == 0 {
			s.firstFailure = at
		}
		s.failures++
		s.lastErr = r.Err
		switch {
		case !s.down && s.failures >= s.threshold:
			s.down, s.lastDown = true, at
			out = append(out, m.downAlert(name, s, at, false))
		case s.down && at.Sub(s.lastDown) >= m.s.Cooldown:
			s.lastDown = at
			out = append(out, m.downAlert(name, s, at, true))
		}
	}

	// WARNs go out at most once per cooldown and never change UP/DOWN.
	if r.Warn != "" && (s.lastWarn.IsZero() || at.Sub(s.lastWarn) >= m.s.Cooldown) {
		s.lastWarn = at
		out = append(out, m.alert(KindWarn, name, "%s", r.Warn))
	}
	if r.Info != "" {
		out = append(out, m.alert(KindInfo, name, "%s", r.Info))
	}
	return out
}

// Summary is the startup message, e.g. "sentinel[fleet] online — 7/7 ✓".
// It lists the failing checks when some did not pass.
func (m *Machine) Summary(passed, total int, failing []string) Alert {
	text := fmt.Sprintf("sentinel[%s] online — %d/%d ✓", m.s.Instance, passed, total)
	if len(failing) > 0 {
		text += " (failing: " + strings.Join(failing, ", ") + ")"
	}
	return Alert{Kind: KindSummary, Text: text}
}

func (m *Machine) downAlert(name string, s *checkState, at time.Time, repeat bool) Alert {
	stamp := at.In(m.s.Location).Format("15:04") + " ET"
	if s.peer {
		verb := "is down"
		if repeat {
			verb = "is still down"
		}
		return m.alert(KindDown, name, "%s %s — %s. %s", name, verb, s.lastErr, stamp)
	}
	verb := "DOWN"
	if repeat {
		verb = "still DOWN"
	}
	noun := "failures"
	if s.failures == 1 {
		noun = "failure"
	}
	return m.alert(KindDown, name, "%s %s — %d consecutive %s (%s). %s", name, verb, s.failures, noun, s.lastErr, stamp)
}

// alert builds the message with its emoji and sentinel[<instance>] prefix.
func (m *Machine) alert(kind Kind, check, format string, args ...any) Alert {
	icon := map[Kind]string{KindDown: "🚨", KindRecovery: "✅", KindWarn: "⚠️", KindInfo: "ℹ️"}[kind]
	text := fmt.Sprintf("%s sentinel[%s]: %s", icon, m.s.Instance, fmt.Sprintf(format, args...))
	return Alert{Kind: kind, Check: check, Text: text}
}
