// Package probe holds the checks sentinel runs. Each check observes and
// reports a Result; none of them ever changes what it watches.
package probe

import (
	"context"
	"fmt"
	"time"
)

// Result is the outcome of one check.
type Result struct {
	OK   bool
	Err  string // why the check failed; empty when OK
	Warn string // a warning that never marks the check DOWN, e.g. a cert about to expire
	Info string // a one-off notice, e.g. a peer rebooted
}

// Fail returns a failed Result with a short reason.
func Fail(format string, args ...any) Result {
	return Result{Err: fmt.Sprintf(format, args...)}
}

// Prober is one check. Check must return when ctx is done; the monitor gives
// every check its own timeout.
type Prober interface {
	Check(ctx context.Context) Result
}

// ProberFunc lets a plain function be used as a Prober.
type ProberFunc func(ctx context.Context) Result

// Check calls f(ctx).
func (f ProberFunc) Check(ctx context.Context) Result { return f(ctx) }

// FormatDuration renders a duration the way alerts show it: "45s", "14m",
// "2h5m", "3d4h". It rounds down to the unit shown.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days, h := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}
