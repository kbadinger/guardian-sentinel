package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kbadinger/guardian-sentinel/internal/probe"
)

// fakeSender records what would have been sent.
type fakeSender struct {
	mu    sync.Mutex
	texts []string
	err   error // returned from every Send
}

func (f *fakeSender) Send(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return f.err
}

func (f *fakeSender) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func fixed(r probe.Result) probe.Prober {
	return probe.ProberFunc(func(context.Context) probe.Result { return r })
}

var settings = Settings{Instance: "fleet", FailThreshold: 3, Cooldown: 6 * time.Hour}

func TestRunSendsStartupSummaryFirst(t *testing.T) {
	sender := &fakeSender{}
	checks := []Check{
		{Name: "a", Prober: fixed(probe.Result{OK: true}), Timeout: time.Second},
		{Name: "b", Prober: fixed(probe.Result{OK: true, Warn: "cert for example.com expires in 9 days"}), Timeout: time.Second},
		{Name: "c", Prober: fixed(probe.Fail("HTTP 500")), Timeout: time.Second},
	}
	r := NewRunner(settings, time.Hour, checks, sender)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return len(sender.sent()) >= 2 })
	cancel()
	<-done // Run must return promptly on shutdown

	want := []string{
		"sentinel[fleet] online — 2/3 ✓ (failing: c)",
		"⚠️ sentinel[fleet]: cert for example.com expires in 9 days",
	}
	if got := sender.sent(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n got  %q\n want %q", got, want)
	}
}

func TestChecksRunConcurrently(t *testing.T) {
	// Each check waits until both have started. Run one after the other,
	// the first would wait until its timeout and fail.
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	barrier := probe.ProberFunc(func(ctx context.Context) probe.Result {
		arrived.Done()
		select {
		case <-both:
			return probe.Result{OK: true}
		case <-ctx.Done():
			return probe.Fail("timed out")
		}
	})
	checks := []Check{
		{Name: "a", Prober: barrier, Timeout: 2 * time.Second},
		{Name: "b", Prober: barrier, Timeout: 2 * time.Second},
	}
	r := NewRunner(settings, time.Hour, checks, &fakeSender{})
	results, _ := r.round(context.Background())
	for i, res := range results {
		if !res.res.OK {
			t.Errorf("check %s: %q (checks did not run concurrently)", checks[i].Name, res.res.Err)
		}
	}
}

func TestEachCheckHasItsOwnTimeout(t *testing.T) {
	hang := probe.ProberFunc(func(ctx context.Context) probe.Result {
		<-ctx.Done()
		return probe.Fail("timed out")
	})
	checks := []Check{
		{Name: "hangs", Prober: hang, Timeout: 20 * time.Millisecond},
		{Name: "fine", Prober: fixed(probe.Result{OK: true}), Timeout: time.Second},
	}
	r := NewRunner(settings, time.Hour, checks, &fakeSender{})

	start := time.Now()
	results, _ := r.round(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("round took %v; the hanging check should have been cut off at its timeout", elapsed)
	}
	if results[0].res.OK || !results[1].res.OK {
		t.Errorf("results = %+v, want hangs failed and fine OK", results)
	}
}

func TestSendFailuresDoNotStopTheLoop(t *testing.T) {
	var calls atomic.Int32
	counting := probe.ProberFunc(func(context.Context) probe.Result {
		calls.Add(1)
		return probe.Fail("connection refused")
	})
	sender := &fakeSender{err: errors.New("telegram unreachable")}
	r := NewRunner(Settings{Instance: "fleet", FailThreshold: 1, Cooldown: time.Nanosecond},
		5*time.Millisecond, []Check{{Name: "site", Prober: counting, Timeout: time.Second}}, sender)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return calls.Load() >= 4 })
	cancel()
	<-done
	if len(sender.sent()) < 3 {
		t.Errorf("expected repeated send attempts despite errors, got %d", len(sender.sent()))
	}
}

// TestHeartbeatPeers runs the receiver and the runner together on a fake
// clock: fresh → silent → alert → resume, for two peers at once.
func TestHeartbeatPeers(t *testing.T) {
	var (
		mu  sync.Mutex
		now = time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }

	key := []byte("test-key-not-a-real-secret")
	var r *Runner
	recv := probe.NewReceiver(key, []probe.Peer{
		{Name: "home-server", MaxSilence: 15 * time.Minute},
		{Name: "laptop", MaxSilence: 15 * time.Minute},
	}, clock, func(peer string, res probe.Result) { r.Report(peer, res) })

	sender := &fakeSender{}
	r = NewRunner(settings, time.Minute, []Check{
		{Name: "home-server", Prober: recv.PeerCheck("home-server"), Timeout: time.Second, Threshold: 1, Peer: true},
		{Name: "laptop", Prober: recv.PeerCheck("laptop"), Timeout: time.Second, Threshold: 1, Peer: true},
	}, sender)
	r.now = clock

	post := func(instance string, uptime int64) {
		body, _ := json.Marshal(probe.Heartbeat{Instance: instance, UptimeS: uptime})
		req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewReader(body))
		probe.SignRequest(req, key, body, clock())
		rec := httptest.NewRecorder()
		recv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("heartbeat from %s: status %d", instance, rec.Code)
		}
	}
	tick := func() {
		_, alerts := r.round(context.Background())
		r.send(context.Background(), alerts)
	}

	tick() // fresh: both inside max_silence since process start
	advance(10 * time.Minute)
	post("laptop", 600)
	advance(6 * time.Minute)
	tick() // home-server silent 16m; laptop heard 6m ago
	advance(time.Minute)
	tick() // still down: no repeat inside the cooldown
	post("home-server", 30)
	r.Wait() // Report sends in the background

	// settings has no Location, so this clock reads in UTC (production uses America/New_York).
	want := []string{
		"🚨 sentinel[fleet]: home-server is down — no heartbeat for 16m. 16:16 ET",
		"✅ sentinel[fleet]: home-server RECOVERED after 1m",
	}
	if got := sender.sent(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n got  %q\n want %q", got, want)
	}
}

func TestHealthyGracePeriod(t *testing.T) {
	tests := []struct {
		interval time.Duration
		limit    time.Duration // max(3 × interval, 2m)
	}{
		{10 * time.Second, 2 * time.Minute},
		{60 * time.Second, 3 * time.Minute},
	}
	for _, tt := range tests {
		start := time.Now()
		r := NewRunner(settings, tt.interval, nil, &fakeSender{})
		// No round has run: the clock counts from process start.
		if !r.Healthy(start.Add(tt.limit - time.Second)) {
			t.Errorf("interval %v: unhealthy inside the %v startup grace", tt.interval, tt.limit)
		}
		if r.Healthy(start.Add(tt.limit + time.Second)) {
			t.Errorf("interval %v: still healthy %v after start with no round", tt.interval, tt.limit+time.Second)
		}
	}
}

// TestHealthzTracksTheLoop wires /healthz to the runner the way main does.
func TestHealthzTracksTheLoop(t *testing.T) {
	r := NewRunner(settings, time.Minute, []Check{
		{Name: "site", Prober: fixed(probe.Result{OK: true}), Timeout: time.Second},
	}, &fakeSender{})
	recv := probe.NewReceiver([]byte("test-key-not-a-real-secret"), nil, nil, nil)
	recv.SetHealthCheck(func() bool { return r.Healthy(time.Now()) })
	h := recv.Handler()
	healthz := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}

	if code := healthz(); code != http.StatusOK {
		t.Errorf("fresh: status %d, want 200", code)
	}
	r.lastRound.Store(time.Now().Add(-4 * time.Minute).UnixNano()) // the loop hung 4m ago (limit 3m)
	if code := healthz(); code != http.StatusServiceUnavailable {
		t.Errorf("stale: status %d, want 503", code)
	}
	r.round(context.Background()) // a completed round makes it healthy again
	if code := healthz(); code != http.StatusOK {
		t.Errorf("after a round: status %d, want 200", code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}
