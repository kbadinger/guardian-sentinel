package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env returns a getenv function backed by a map.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

const testToken = "test-heartbeat-secret-value"

var testEnv = env(map[string]string{"SENTINEL_HEARTBEAT_TOKEN": testToken})

func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "deploy", "fleet-config.example.json"), testEnv)
	if err != nil {
		t.Fatalf("example config should load: %v", err)
	}
	if c.Instance != "fleet" {
		t.Errorf("instance = %q, want fleet", c.Instance)
	}
	var sawHeartbeat bool
	for _, p := range c.Probes {
		if p.Type == TypeHeartbeat {
			sawHeartbeat = true
			if p.Token != testToken {
				t.Errorf("heartbeat token was not resolved from the environment")
			}
		}
	}
	if !sawHeartbeat {
		t.Error("example config should include a heartbeat receiver")
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`{
		"instance": "fleet",
		"probes": [{"name": "site", "type": "http", "url": "https://example.com/", "expect": "Example"}]
	}`), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"interval", c.Interval.Duration, DefaultInterval},
		{"probe_timeout", c.ProbeTimeout.Duration, DefaultProbeTimeout},
		{"fail_threshold", c.FailThreshold, DefaultFailThreshold},
		{"realert_cooldown", c.RealertCooldown.Duration, DefaultRealertCooldown},
		{"warn_cert_days", *c.Probes[0].WarnCertDays, DefaultWarnCertDays},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
}

func TestExplicitValuesKept(t *testing.T) {
	c, err := Parse([]byte(`{
		"instance": "fleet", "interval": "30s", "probe_timeout": "5s", "fail_threshold": 2, "realert_cooldown": "1h",
		"probes": [{"name": "site", "type": "http", "url": "https://example.com/", "expect": "Example", "warn_cert_days": 0, "timeout": "3s"}]
	}`), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.Duration != 30*time.Second || c.ProbeTimeout.Duration != 5*time.Second ||
		c.FailThreshold != 2 || c.RealertCooldown.Duration != time.Hour {
		t.Errorf("top-level values not kept: %+v", c)
	}
	p := c.Probes[0]
	if *p.WarnCertDays != 0 {
		t.Errorf("warn_cert_days 0 should stay 0 (cert warnings off), got %d", *p.WarnCertDays)
	}
	if p.Timeout.Duration != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", p.Timeout.Duration)
	}
}

func TestInvalidConfigs(t *testing.T) {
	const site = `{"name": "site", "type": "http", "url": "https://example.com/", "expect": "Example"}`
	wrap := func(probes string) string {
		return `{"instance": "fleet", "probes": [` + probes + `]}`
	}
	heartbeat := func(tokenEnv, peers string) string {
		return `{"name": "hb", "type": "heartbeat", "listen": ":8080", "token_env": "` + tokenEnv + `", "peers": [` + peers + `]}`
	}
	const peer = `{"name": "home-server", "max_silence": "15m"}`

	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{"not json", `{`, "parse config"},
		{"unknown field", `{"instance": "fleet", "intervall": "60s", "probes": [` + site + `]}`, `unknown field "intervall"`},
		{"missing instance", `{"probes": [` + site + `]}`, "instance is required"},
		{"no probes", `{"instance": "fleet", "probes": []}`, "at least one probe"},
		{"bad duration", `{"instance": "fleet", "interval": "soon", "probes": [` + site + `]}`, `invalid duration "soon"`},
		{"duration not a string", `{"instance": "fleet", "interval": 60, "probes": [` + site + `]}`, "duration must be a string"},
		{"negative interval", `{"instance": "fleet", "interval": "-1s", "probes": [` + site + `]}`, "interval must be positive"},
		{"negative fail_threshold", `{"instance": "fleet", "fail_threshold": -1, "probes": [` + site + `]}`, "fail_threshold"},
		{"unknown probe type", wrap(`{"name": "x", "type": "ping"}`), `unknown type "ping"`},
		{"not yet built probe type", wrap(`{"name": "x", "type": "speedtest"}`), `unknown type "speedtest"`},
		{"missing type", wrap(`{"name": "x"}`), "type is required"},
		{"missing name", wrap(`{"type": "http", "url": "https://example.com/", "expect": "Example"}`), "name is required"},
		{"duplicate name", wrap(site + "," + site), `duplicate name "site"`},
		{"http missing url", wrap(`{"name": "x", "type": "http", "expect": "Example"}`), "url is required"},
		{"http relative url", wrap(`{"name": "x", "type": "http", "url": "example.com", "expect": "Example"}`), "absolute http"},
		{"http wrong scheme", wrap(`{"name": "x", "type": "http", "url": "ftp://example.com/", "expect": "Example"}`), "absolute http"},
		{"http missing expect", wrap(`{"name": "x", "type": "http", "url": "https://example.com/"}`), "expect is required"},
		{"http negative warn_cert_days", wrap(`{"name": "x", "type": "http", "url": "https://example.com/", "expect": "E", "warn_cert_days": -1}`), "warn_cert_days"},
		{"heartbeat missing listen", wrap(`{"name": "hb", "type": "heartbeat", "token_env": "SENTINEL_HEARTBEAT_TOKEN", "peers": [` + peer + `]}`), "listen is required"},
		{"heartbeat bad listen", wrap(`{"name": "hb", "type": "heartbeat", "listen": "8080", "token_env": "SENTINEL_HEARTBEAT_TOKEN", "peers": [` + peer + `]}`), "listen must be"},
		{"heartbeat missing token_env", wrap(heartbeat("", peer)), "token_env is required"},
		{"heartbeat env var not set", wrap(heartbeat("SOME_UNSET_VAR", peer)), "SOME_UNSET_VAR (token_env) is not set"},
		{"heartbeat peer missing name", wrap(heartbeat("SENTINEL_HEARTBEAT_TOKEN", `{"max_silence": "15m"}`)), "peer #1: name is required"},
		{"heartbeat peer missing max_silence", wrap(heartbeat("SENTINEL_HEARTBEAT_TOKEN", `{"name": "home-server"}`)), "max_silence is required"},
		{"peer name clashes with probe", wrap(site + "," + heartbeat("SENTINEL_HEARTBEAT_TOKEN", `{"name": "site", "max_silence": "15m"}`)), `duplicate name "site"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json), testEnv)
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSecretErrorsNeverPrintValues(t *testing.T) {
	// Someone pasted the secret itself into token_env: the error must not echo it.
	pasted := "s3cr3t-value-with-dashes"
	cfg := `{"instance": "fleet", "probes": [{"name": "hb", "type": "heartbeat", "listen": ":8080", "token_env": "` +
		pasted + `", "peers": [{"name": "home-server", "max_silence": "15m"}]}]}`
	_, err := Parse([]byte(cfg), env(map[string]string{pasted: "anything"}))
	if err == nil {
		t.Fatal("expected an error for a token_env that is not a variable name")
	}
	if strings.Contains(err.Error(), pasted) {
		t.Errorf("error echoes the token_env value: %q", err)
	}

	// Any other config error must not print a resolved secret either.
	cfg = `{"instance": "", "probes": [{"name": "hb", "type": "heartbeat", "listen": ":8080", "token_env": "SENTINEL_HEARTBEAT_TOKEN",
		"peers": [{"name": "home-server", "max_silence": "15m"}]}]}`
	_, err = Parse([]byte(cfg), testEnv)
	if err == nil {
		t.Fatal("expected an error for the missing instance")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error prints the secret: %q", err)
	}
}

func TestReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`{"probes": [{"name": "x", "type": "http"}]}`), testEnv)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"instance is required", "url is required", "expect is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"), testEnv)
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("expected a read error, got %v", err)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"instance": "test", "probes": [{"name": "site", "type": "http", "url": "http://127.0.0.1:1/", "expect": "ok"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Probes[0].URL != "http://127.0.0.1:1/" {
		t.Errorf("url = %q", c.Probes[0].URL)
	}
}

func TestIsEnvName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"SENTINEL_HEARTBEAT_TOKEN", true},
		{"_X1", true},
		{"lower_ok", true},
		{"", false},
		{"1ABC", false},
		{"HAS-DASH", false},
		{"HAS SPACE", false},
	}
	for _, tt := range tests {
		if got := isEnvName(tt.in); got != tt.want {
			t.Errorf("isEnvName(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// A receiver with no peers yet still serves /healthz for the Kubernetes
// liveness probe; peers are added as each on-box instance starts sending.
func TestHeartbeatWithNoPeersIsValid(t *testing.T) {
	data := `{"instance": "fleet", "probes": [{"name": "hb", "type": "heartbeat", "listen": ":8080", "token_env": "SENTINEL_HEARTBEAT_TOKEN", "peers": []}]}`
	c, err := Parse([]byte(data), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Probes[0].Peers) != 0 {
		t.Errorf("peers = %v, want none", c.Probes[0].Peers)
	}
}
