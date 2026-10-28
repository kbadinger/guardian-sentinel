package monitor

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kbadinger/guardian-sentinel/internal/probe"
)

// sendTimeout bounds one alert delivery.
const sendTimeout = 15 * time.Second

// minStallLimit is the shortest gap without a completed round that Healthy
// accepts; see Runner.Healthy.
const minStallLimit = 2 * time.Minute

// Sender delivers an alert's text (Telegram, or stdout in dry-run).
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Check is one thing the Runner checks every tick.
type Check struct {
	Name      string
	Prober    probe.Prober
	Timeout   time.Duration // per-check context timeout
	Threshold int           // consecutive failures before DOWN; 0 means the default
	Peer      bool          // a heartbeat peer (see Machine.Track)
}

// Runner runs every check concurrently once per interval, feeds the results
// to a Machine and sends the alerts it returns.
type Runner struct {
	checks   []Check
	interval time.Duration
	sender   Sender
	now      func() time.Time

	mu      sync.Mutex // guards machine: ticks and heartbeat POSTs both feed it
	machine *Machine

	sends sync.WaitGroup // alert deliveries started by Report

	lastRound atomic.Int64 // unix nanos of the last completed round; process start until the first one
}

// NewRunner returns a Runner for checks.
func NewRunner(s Settings, interval time.Duration, checks []Check, sender Sender) *Runner {
	m := NewMachine(s)
	for _, c := range checks {
		m.Track(c.Name, c.Threshold, c.Peer)
	}
	r := &Runner{checks: checks, interval: interval, sender: sender, now: time.Now, machine: m}
	r.lastRound.Store(time.Now().UnixNano())
	return r
}

// Healthy reports whether the loop is still completing rounds: false when
// no round has completed within max(3 × interval, 2m) of now, counting from
// process start until the first round finishes. /healthz serves this so
// Kubernetes restarts a hung monitor, not just a crashed one.
func (r *Runner) Healthy(now time.Time) bool {
	limit := max(3*r.interval, minStallLimit)
	return now.Sub(time.Unix(0, r.lastRound.Load())) <= limit
}

// Run does one round straight away and sends the startup summary, then a
// round every interval until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	results, alerts := r.round(ctx)
	if ctx.Err() != nil {
		return
	}
	r.send(ctx, append([]Alert{r.summary(results)}, alerts...))

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, alerts := r.round(ctx)
			if ctx.Err() != nil {
				return // results from a round cut short by shutdown are not real failures
			}
			r.send(ctx, alerts)
		}
	}
}

// Report feeds a result that arrived on its own, such as an accepted
// heartbeat, into the state machine. Alerts are sent in the background so
// the HTTP handler that called Report can answer straight away.
func (r *Runner) Report(name string, res probe.Result) {
	at := r.now()
	r.mu.Lock()
	alerts := r.machine.Observe(name, res, at)
	r.mu.Unlock()
	if len(alerts) == 0 {
		return
	}
	r.sends.Add(1)
	go func() {
		defer r.sends.Done()
		r.send(context.Background(), alerts)
	}()
}

// Wait blocks until alerts started by Report have been delivered. Call it
// on shutdown after the HTTP servers that call Report have stopped.
func (r *Runner) Wait() {
	r.sends.Wait()
}

// timedResult is a check's result plus when the check started.
type timedResult struct {
	at  time.Time
	res probe.Result
}

// round runs every check concurrently, each under its own timeout, then
// applies all results to the machine.
func (r *Runner) round(ctx context.Context) ([]timedResult, []Alert) {
	results := make([]timedResult, len(r.checks))
	var wg sync.WaitGroup
	for i, c := range r.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, c.Timeout)
			defer cancel()
			at := r.now() // the start time, so a heartbeat that lands mid-check wins
			results[i] = timedResult{at: at, res: c.Prober.Check(cctx)}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, nil
	}

	var alerts []Alert
	r.mu.Lock()
	for i, c := range r.checks {
		if !results[i].res.OK {
			log.Printf("check %s failed: %s", c.Name, results[i].res.Err)
		}
		alerts = append(alerts, r.machine.Observe(c.Name, results[i].res, results[i].at)...)
	}
	r.mu.Unlock()
	r.lastRound.Store(time.Now().UnixNano())
	return results, alerts
}

func (r *Runner) summary(results []timedResult) Alert {
	var failing []string
	for i, c := range r.checks {
		if !results[i].res.OK {
			failing = append(failing, c.Name)
		}
	}
	return r.machine.Summary(len(r.checks)-len(failing), len(r.checks), failing)
}

// send delivers alerts in order. A failed delivery is logged, with the
// alert text so it is not lost, and the loop carries on: a Telegram outage
// must never stop the monitoring.
func (r *Runner) send(ctx context.Context, alerts []Alert) {
	for _, a := range alerts {
		// Not tied to ctx's cancellation: an alert that is already due still
		// goes out during a shutdown.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
		if err := r.sender.Send(sctx, a.Text); err != nil {
			log.Printf("alert send failed: %v (alert was: %s)", err, a.Text)
		}
		cancel()
	}
}
