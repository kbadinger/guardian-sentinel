package probe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Heartbeat wire format. A sender POSTs a JSON Heartbeat body to
// /heartbeat with two headers:
//
//	X-Sentinel-Timestamp: unix seconds when the request was signed
//	X-Sentinel-Signature: hex HMAC-SHA256(key, timestamp + "." + body)
//
// The receiver rejects a timestamp more than MaxClockSkew from its own
// clock, and, per peer, any timestamp at or below the last one it accepted,
// so a captured request cannot be replayed. The endpoint runs
// over plain HTTP on a public port, which is why a static bearer token
// (sniffable, replayable) is not enough.
const (
	TimestampHeader = "X-Sentinel-Timestamp"
	SignatureHeader = "X-Sentinel-Signature"
	MaxClockSkew    = 5 * time.Minute
	maxBeatBytes    = 4 << 10 // 4 KiB is plenty for {"instance": ..., "uptime_s": ...}
)

// Heartbeat is the body a peer POSTs.
type Heartbeat struct {
	Instance string `json:"instance"`
	UptimeS  int64  `json:"uptime_s"`
}

// Sign returns the hex HMAC-SHA256 of timestamp + "." + body.
func Sign(key []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignRequest sets the timestamp and signature headers on a heartbeat
// request. body must be exactly the bytes sent as the request body.
// This is the sender side; the heartbeat_send probe uses it.
func SignRequest(req *http.Request, key, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, Sign(key, ts, body))
}

// Peer is one machine the receiver expects heartbeats from.
type Peer struct {
	Name       string
	MaxSilence time.Duration
}

// Receiver accepts signed heartbeats and tracks when each peer was last
// heard from. Every peer's clock starts at process start, so a restart
// (including a rolling deploy) gives each peer a fresh max_silence window.
type Receiver struct {
	key     []byte
	now     func() time.Time
	onBeat  func(peer string, r Result)
	healthy func() bool // nil means always healthy; see SetHealthCheck

	mu    sync.Mutex
	peers map[string]*peerState
}

type peerState struct {
	maxSilence time.Duration
	lastSeen   time.Time
	uptime     int64
	heard      bool  // a valid heartbeat has arrived since process start
	lastTS     int64 // signed timestamp of the last accepted heartbeat
}

// NewReceiver returns a receiver for peers, authenticated with key.
// onBeat is called after every accepted heartbeat with an OK Result
// (carrying Info when the peer rebooted); it may be nil. now may be nil
// (time.Now); tests pass a fake clock.
func NewReceiver(key []byte, peers []Peer, now func() time.Time, onBeat func(peer string, r Result)) *Receiver {
	if now == nil {
		now = time.Now
	}
	start := now()
	r := &Receiver{key: key, now: now, onBeat: onBeat, peers: map[string]*peerState{}}
	for _, p := range peers {
		r.peers[p.Name] = &peerState{maxSilence: p.MaxSilence, lastSeen: start}
	}
	return r
}

// SetHealthCheck sets the function /healthz asks. The monitor passes one
// that reports whether its loop is still completing rounds. Call it before
// serving Handler.
func (r *Receiver) SetHealthCheck(healthy func() bool) {
	r.healthy = healthy
}

// Handler serves POST /heartbeat and GET /healthz. /healthz is the
// Kubernetes liveness probe: 200 while the health check passes, 503 when it
// fails (a hung monitor loop), so Kubernetes restarts the pod.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /heartbeat", r.handleHeartbeat)
	mux.HandleFunc("GET /healthz", r.handleHealthz)
	return mux
}

func (r *Receiver) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.healthy != nil && !r.healthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "monitor loop stalled\n")
		return
	}
	io.WriteString(w, "ok\n")
}

// PeerCheck returns a Prober that fails once the named peer has been silent
// longer than its max_silence.
func (r *Receiver) PeerCheck(name string) Prober {
	return ProberFunc(func(context.Context) Result {
		r.mu.Lock()
		p, ok := r.peers[name]
		var silent time.Duration
		if ok {
			silent = r.now().Sub(p.lastSeen)
		}
		r.mu.Unlock()

		switch {
		case !ok:
			return Fail("unknown peer")
		case silent > p.maxSilence:
			return Fail("no heartbeat for %s", FormatDuration(silent))
		default:
			return Result{OK: true}
		}
	})
}

func (r *Receiver) handleHeartbeat(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBeatBytes))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Authenticate before looking at the body at all.
	ts, ok := r.verify(req.Header.Get(TimestampHeader), req.Header.Get(SignatureHeader), body)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var hb Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		http.Error(w, "bad heartbeat body", http.StatusBadRequest)
		return
	}

	result, outcome := r.record(hb, ts)
	switch outcome {
	case unknownPeer:
		http.Error(w, "unknown instance", http.StatusForbidden)
		return
	case replayed:
		http.Error(w, "replayed or out-of-order heartbeat", http.StatusUnauthorized)
		return
	}
	if r.onBeat != nil {
		r.onBeat(hb.Instance, result)
	}
	w.WriteHeader(http.StatusNoContent)
}

// verify checks the timestamp window and the signature, and returns the
// signed timestamp. The signature is compared in constant time so its bytes
// cannot be guessed by timing.
func (r *Receiver) verify(timestamp, signature string, body []byte) (int64, bool) {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return 0, false
	}
	skew := r.now().Sub(time.Unix(ts, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return 0, false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return 0, false
	}
	want, _ := hex.DecodeString(Sign(r.key, timestamp, body))
	return ts, subtle.ConstantTimeCompare(got, want) == 1
}

// recordOutcome is what record did with a heartbeat.
type recordOutcome int

const (
	accepted recordOutcome = iota
	unknownPeer
	replayed // timestamp at or below the last accepted one for this peer
)

// record marks a peer as heard from now. It reports a reboot when uptime_s
// went down since the last heartbeat.
func (r *Receiver) record(hb Heartbeat, ts int64) (Result, recordOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[hb.Instance]
	if !ok {
		return Result{}, unknownPeer
	}
	if p.heard && ts <= p.lastTS {
		return Result{}, replayed
	}
	result := Result{OK: true}
	if p.heard && hb.UptimeS < p.uptime {
		up := time.Duration(hb.UptimeS) * time.Second
		result.Info = hb.Instance + " rebooted (up " + FormatDuration(up) + ")"
	}
	p.lastSeen = r.now()
	p.uptime = hb.UptimeS
	p.lastTS = ts
	p.heard = true
	return result, accepted
}
