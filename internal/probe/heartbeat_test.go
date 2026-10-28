package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = []byte("test-key-not-a-real-secret")

// fakeClock is a settable clock for tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// beat is one call to the receiver's onBeat callback.
type beat struct {
	peer string
	r    Result
}

func newTestReceiver(clock *fakeClock) (*Receiver, *[]beat) {
	var beats []beat
	peers := []Peer{{Name: "home-server", MaxSilence: 15 * time.Minute}, {Name: "laptop", MaxSilence: 30 * time.Minute}}
	recv := NewReceiver(testKey, peers, clock.Now, func(peer string, r Result) {
		beats = append(beats, beat{peer, r})
	})
	return recv, &beats
}

// signedRequest builds a heartbeat request the way a sender would.
func signedRequest(t *testing.T, key []byte, hb Heartbeat, now time.Time) *http.Request {
	t.Helper()
	body, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewReader(body))
	SignRequest(req, key, body, now)
	return req
}

func serve(h http.Handler, req *http.Request) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestSignRoundTrip(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
	recv, beats := newTestReceiver(clock)

	code := serve(recv.Handler(), signedRequest(t, testKey, Heartbeat{Instance: "home-server", UptimeS: 600}, clock.Now()))
	if code != http.StatusNoContent {
		t.Fatalf("signed heartbeat: status %d, want 204", code)
	}
	if len(*beats) != 1 || (*beats)[0].peer != "home-server" || !(*beats)[0].r.OK {
		t.Errorf("onBeat calls = %+v, want one OK beat for home-server", *beats)
	}
}

func TestHeartbeatRejections(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	good := Heartbeat{Instance: "home-server", UptimeS: 600}

	tests := []struct {
		name string
		req  func(t *testing.T) *http.Request
		want int
	}{
		{"wrong key", func(t *testing.T) *http.Request {
			return signedRequest(t, []byte("some-other-key"), good, now)
		}, http.StatusUnauthorized},
		{"timestamp too old", func(t *testing.T) *http.Request {
			return signedRequest(t, testKey, good, now.Add(-MaxClockSkew-time.Second))
		}, http.StatusUnauthorized},
		{"timestamp too far ahead", func(t *testing.T) *http.Request {
			return signedRequest(t, testKey, good, now.Add(MaxClockSkew+time.Second))
		}, http.StatusUnauthorized},
		{"body changed after signing", func(t *testing.T) *http.Request {
			req := signedRequest(t, testKey, good, now)
			tampered := `{"instance":"home-server","uptime_s":1}`
			req.Body = io.NopCloser(strings.NewReader(tampered))
			return req
		}, http.StatusUnauthorized},
		{"no signature headers", func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/heartbeat", strings.NewReader(`{"instance":"home-server"}`))
		}, http.StatusUnauthorized},
		{"signature not hex", func(t *testing.T) *http.Request {
			req := signedRequest(t, testKey, good, now)
			req.Header.Set(SignatureHeader, "not-hex")
			return req
		}, http.StatusUnauthorized},
		{"unknown instance", func(t *testing.T) *http.Request {
			return signedRequest(t, testKey, Heartbeat{Instance: "stranger", UptimeS: 1}, now)
		}, http.StatusForbidden},
		{"signed but not JSON", func(t *testing.T) *http.Request {
			body := []byte("hello")
			req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewReader(body))
			SignRequest(req, testKey, body, now)
			return req
		}, http.StatusBadRequest},
		{"body too large", func(t *testing.T) *http.Request {
			body := bytes.Repeat([]byte("x"), maxBeatBytes+1)
			req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewReader(body))
			SignRequest(req, testKey, body, now)
			return req
		}, http.StatusRequestEntityTooLarge},
		{"GET is not allowed", func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/heartbeat", nil)
		}, http.StatusMethodNotAllowed},
		{"within the window is fine", func(t *testing.T) *http.Request {
			return signedRequest(t, testKey, good, now.Add(-MaxClockSkew+time.Second))
		}, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{t: now}
			recv, beats := newTestReceiver(clock)
			if code := serve(recv.Handler(), tt.req(t)); code != tt.want {
				t.Errorf("status %d, want %d", code, tt.want)
			}
			if tt.want != http.StatusNoContent && len(*beats) != 0 {
				t.Errorf("a rejected heartbeat must not count: %+v", *beats)
			}
		})
	}
}

func TestPeerSilence(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
	recv, _ := newTestReceiver(clock)
	h := recv.Handler()
	home, laptop := recv.PeerCheck("home-server"), recv.PeerCheck("laptop")
	ctx := context.Background()

	// Fresh: timers start at process start, so nobody is late yet.
	if !home.Check(ctx).OK || !laptop.Check(ctx).OK {
		t.Fatal("peers should be OK right after start")
	}

	// 16 minutes in, laptop has checked in but home-server has not.
	clock.Advance(16 * time.Minute)
	serve(h, signedRequest(t, testKey, Heartbeat{Instance: "laptop", UptimeS: 100}, clock.Now()))
	if r := home.Check(ctx); r.OK || r.Err != "no heartbeat for 16m" {
		t.Errorf("home-server: got %+v, want 'no heartbeat for 16m'", r)
	}
	if !laptop.Check(ctx).OK {
		t.Error("one silent peer must not affect another")
	}

	// home-server comes back.
	serve(h, signedRequest(t, testKey, Heartbeat{Instance: "home-server", UptimeS: 50}, clock.Now()))
	if !home.Check(ctx).OK {
		t.Error("home-server should be OK after a valid heartbeat")
	}

	if r := recv.PeerCheck("nobody").Check(ctx); r.OK {
		t.Error("an unknown peer check should fail")
	}
}

func TestRebootDetection(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
	recv, beats := newTestReceiver(clock)
	h := recv.Handler()

	for _, uptime := range []int64{1000, 1300, 180} {
		serve(h, signedRequest(t, testKey, Heartbeat{Instance: "home-server", UptimeS: uptime}, clock.Now()))
		clock.Advance(5 * time.Minute)
	}
	want := []string{"", "", "home-server rebooted (up 3m)"}
	if len(*beats) != len(want) {
		t.Fatalf("got %d beats, want %d", len(*beats), len(want))
	}
	for i, b := range *beats {
		if b.r.Info != want[i] {
			t.Errorf("beat %d info = %q, want %q", i, b.r.Info, want[i])
		}
	}
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name    string
		healthy func() bool
		want    int
	}{
		{"no health check set", nil, http.StatusOK},
		{"monitor loop healthy", func() bool { return true }, http.StatusOK},
		{"monitor loop stalled", func() bool { return false }, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := NewReceiver(testKey, nil, nil, nil)
			recv.SetHealthCheck(tt.healthy)
			srv := httptest.NewServer(recv.Handler())
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("/healthz status %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestReplayRejected(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
	recv, beats := newTestReceiver(clock)
	h := recv.Handler()
	hb := Heartbeat{Instance: "home-server", UptimeS: 600}

	first := signedRequest(t, testKey, hb, clock.Now())
	replay := signedRequest(t, testKey, hb, clock.Now()) // an exact copy: same timestamp, same signature
	older := signedRequest(t, testKey, hb, clock.Now().Add(-time.Minute))
	clock.Advance(time.Minute)
	newer := signedRequest(t, testKey, hb, clock.Now())
	other := signedRequest(t, testKey, Heartbeat{Instance: "laptop", UptimeS: 5}, clock.Now().Add(-2*time.Minute))

	steps := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"first heartbeat", first, http.StatusNoContent},
		{"same timestamp again", replay, http.StatusUnauthorized},
		{"older timestamp", older, http.StatusUnauthorized},
		{"newer timestamp", newer, http.StatusNoContent},
		{"another peer keeps its own timestamps", other, http.StatusNoContent},
	}
	for _, s := range steps {
		if code := serve(h, s.req); code != s.want {
			t.Errorf("%s: status %d, want %d", s.name, code, s.want)
		}
	}
	if len(*beats) != 3 {
		t.Errorf("got %d accepted heartbeats, want 3", len(*beats))
	}
}

func TestSignIsDeterministic(t *testing.T) {
	body := []byte(`{"instance":"home-server","uptime_s":1}`)
	ts := strconv.FormatInt(1785240000, 10)
	a, b := Sign(testKey, ts, body), Sign(testKey, ts, body)
	if a != b || len(a) != 64 {
		t.Errorf("Sign should be a stable 64-char hex digest, got %q and %q", a, b)
	}
	if Sign(testKey, ts+"1", body) == a {
		t.Error("the timestamp must be part of the signature")
	}
}
