package monitor

import (
	"reflect"
	"testing"
	"time"

	"github.com/kbadinger/guardian-sentinel/internal/probe"
)

// t0 is 12:41 in New York (EDT, UTC-4).
var t0 = time.Date(2026, 7, 28, 16, 41, 0, 0, time.UTC)

func newTestMachine(t *testing.T) *Machine {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return NewMachine(Settings{Instance: "fleet", FailThreshold: 3, Cooldown: 6 * time.Hour, Location: ny})
}

// step is one result fed to the machine, offset minutes after t0.
type step struct {
	check  string
	at     time.Duration
	result probe.Result
}

var (
	ok      = probe.Result{OK: true}
	refused = probe.Fail("connection refused")
)

func mins(m int) time.Duration { return time.Duration(m) * time.Minute }

func TestMachine(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
		want  []string // alert texts, in order, across all steps
	}{
		{
			name:  "a blip below the threshold is silent",
			steps: []step{{"site", 0, refused}, {"site", mins(1), refused}, {"site", mins(2), ok}},
			want:  nil,
		},
		{
			name: "DOWN at the threshold, with the time in ET",
			steps: []step{
				{"site", 0, refused}, {"site", mins(1), refused}, {"site", mins(2), refused},
				{"site", mins(3), refused},
			},
			want: []string{"🚨 sentinel[fleet]: site DOWN — 3 consecutive failures (connection refused). 12:43 ET"},
		},
		{
			name: "RECOVERY reports the outage from its first failure",
			steps: []step{
				{"site", 0, refused}, {"site", mins(1), refused}, {"site", mins(2), refused},
				{"site", mins(14), ok}, {"site", mins(15), ok},
			},
			want: []string{
				"🚨 sentinel[fleet]: site DOWN — 3 consecutive failures (connection refused). 12:43 ET",
				"✅ sentinel[fleet]: site RECOVERED after 14m",
			},
		},
		{
			name: "re-alert only after the cooldown",
			steps: []step{
				{"site", 0, refused}, {"site", mins(1), refused}, {"site", mins(2), refused},
				{"site", mins(60), refused},                     // 58m after the DOWN alert: quiet
				{"site", mins(2 + 360), refused},                // 6h after: re-alert
				{"site", mins(3 + 360), refused},                // quiet again
				{"site", mins(2 + 720), probe.Fail("HTTP 502")}, // next 6h: re-alert with the latest reason
			},
			want: []string{
				"🚨 sentinel[fleet]: site DOWN — 3 consecutive failures (connection refused). 12:43 ET",
				"🚨 sentinel[fleet]: site still DOWN — 5 consecutive failures (connection refused). 18:43 ET",
				"🚨 sentinel[fleet]: site still DOWN — 7 consecutive failures (HTTP 502). 00:43 ET",
			},
		},
		{
			name: "a success resets the failure count",
			steps: []step{
				{"site", 0, refused}, {"site", mins(1), refused}, {"site", mins(2), ok},
				{"site", mins(3), refused}, {"site", mins(4), refused},
			},
			want: nil,
		},
		{
			name: "WARN once per cooldown and never DOWN",
			steps: []step{
				{"site", 0, probe.Result{OK: true, Warn: "cert for example.com expires in 9 days"}},
				{"site", mins(1), probe.Result{OK: true, Warn: "cert for example.com expires in 9 days"}},
				{"site", mins(60), probe.Result{OK: true, Warn: "cert for example.com expires in 9 days"}},
				{"site", mins(360), probe.Result{OK: true, Warn: "cert for example.com expires in 8 days"}},
				{"site", mins(361), ok}, // no RECOVERY: a warning never marked it down
			},
			want: []string{
				"⚠️ sentinel[fleet]: cert for example.com expires in 9 days",
				"⚠️ sentinel[fleet]: cert for example.com expires in 8 days",
			},
		},
		{
			name: "a failing check can warn too",
			steps: []step{
				{"site", 0, probe.Result{Err: "HTTP 500", Warn: "cert for example.com expires in 2 days"}},
			},
			want: []string{"⚠️ sentinel[fleet]: cert for example.com expires in 2 days"},
		},
		{
			name: "checks are independent",
			steps: []step{
				{"a", 0, refused}, {"b", 0, ok},
				{"a", mins(1), refused}, {"b", mins(1), refused},
				{"a", mins(2), refused}, {"b", mins(2), ok},
				{"a", mins(3), ok}, {"b", mins(3), refused},
			},
			want: []string{
				"🚨 sentinel[fleet]: a DOWN — 3 consecutive failures (connection refused). 12:43 ET",
				"✅ sentinel[fleet]: a RECOVERED after 3m",
			},
		},
		{
			name: "a heartbeat peer goes down on its first failure",
			steps: []step{
				{"home-server", 0, ok},
				{"home-server", mins(16), probe.Fail("no heartbeat for 16m")},
				{"home-server", mins(17), probe.Fail("no heartbeat for 17m")},
				{"home-server", mins(20), probe.Result{OK: true, Info: "home-server rebooted (up 1m)"}},
			},
			want: []string{
				"🚨 sentinel[fleet]: home-server is down — no heartbeat for 16m. 12:57 ET",
				"✅ sentinel[fleet]: home-server RECOVERED after 4m",
				"ℹ️ sentinel[fleet]: home-server rebooted (up 1m)",
			},
		},
		{
			name: "a stale result does not undo a newer one",
			steps: []step{
				{"home-server", mins(16), probe.Fail("no heartbeat for 16m")},
				{"home-server", mins(18), ok},                                 // heartbeat arrives: RECOVERED
				{"home-server", mins(17), probe.Fail("no heartbeat for 17m")}, // measured before it: ignored
				{"home-server", mins(19), ok},
			},
			want: []string{
				"🚨 sentinel[fleet]: home-server is down — no heartbeat for 16m. 12:57 ET",
				"✅ sentinel[fleet]: home-server RECOVERED after 2m",
			},
		},
		{
			name: "an untracked check uses the default threshold",
			steps: []step{
				{"new", 0, refused}, {"new", mins(1), refused}, {"new", mins(2), refused},
			},
			want: []string{"🚨 sentinel[fleet]: new DOWN — 3 consecutive failures (connection refused). 12:43 ET"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestMachine(t)
			m.Track("site", 0, false)
			m.Track("a", 0, false)
			m.Track("b", 0, false)
			m.Track("home-server", 1, true)

			var got []string
			for _, s := range tt.steps {
				for _, a := range m.Observe(s.check, s.result, t0.Add(s.at)) {
					got = append(got, a.Text)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("alerts:\n got  %q\n want %q", got, tt.want)
			}
		})
	}
}

func TestMachineSingleFailureThreshold(t *testing.T) {
	m := newTestMachine(t)
	m.Track("site", 1, false)
	alerts := m.Observe("site", refused, t0)
	want := "🚨 sentinel[fleet]: site DOWN — 1 consecutive failure (connection refused). 12:41 ET"
	if len(alerts) != 1 || alerts[0].Text != want || alerts[0].Kind != KindDown {
		t.Errorf("got %+v, want one DOWN alert %q", alerts, want)
	}
}

func TestSummary(t *testing.T) {
	m := newTestMachine(t)
	tests := []struct {
		passed, total int
		failing       []string
		want          string
	}{
		{7, 7, nil, "sentinel[fleet] online — 7/7 ✓"},
		{5, 7, []string{"a", "b"}, "sentinel[fleet] online — 5/7 ✓ (failing: a, b)"},
	}
	for _, tt := range tests {
		if got := m.Summary(tt.passed, tt.total, tt.failing); got.Text != tt.want || got.Kind != KindSummary {
			t.Errorf("got %+v, want %q", got, tt.want)
		}
	}
}
