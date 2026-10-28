// Package config loads and validates a sentinel JSON config file.
//
// The config declares probes; the environment carries secrets. A probe that
// needs a secret names the environment variable that holds it (token_env),
// and Load resolves it. No secret value ever appears in a config file or in
// an error message.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

// Probe types this build supports. Later types (mcp, backup, heartbeat_send,
// exec, speedtest) are added here and in validateProbe.
const (
	TypeHTTP      = "http"
	TypeHeartbeat = "heartbeat"
)

// Defaults from SPEC.md.
const (
	DefaultInterval        = 60 * time.Second
	DefaultProbeTimeout    = 10 * time.Second
	DefaultFailThreshold   = 3
	DefaultRealertCooldown = 6 * time.Hour
	DefaultWarnCertDays    = 14
)

// Config is one sentinel instance's config file.
type Config struct {
	Instance        string   `json:"instance"`
	Interval        Duration `json:"interval"`
	ProbeTimeout    Duration `json:"probe_timeout"`
	FailThreshold   int      `json:"fail_threshold"`
	RealertCooldown Duration `json:"realert_cooldown"`
	Probes          []Probe  `json:"probes"`
}

// Probe is one entry in "probes". Which fields apply depends on Type.
type Probe struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// http
	URL          string   `json:"url"`
	Expect       string   `json:"expect"`
	WarnCertDays *int     `json:"warn_cert_days"` // nil means DefaultWarnCertDays; 0 turns cert warnings off
	Timeout      Duration `json:"timeout"`        // overrides probe_timeout for this probe

	// heartbeat (receiver)
	Listen   string `json:"listen"`
	TokenEnv string `json:"token_env"`
	Peers    []Peer `json:"peers"`

	// Token is the secret read from the TokenEnv environment variable.
	// It is never part of the JSON.
	Token string `json:"-"`
}

// Peer is one machine the heartbeat receiver expects to hear from.
type Peer struct {
	Name       string   `json:"name"`
	MaxSilence Duration `json:"max_silence"`
}

// Load reads the config file at path and validates it. getenv resolves
// token_env names (pass os.Getenv; tests pass a map lookup).
func Load(path string, getenv func(string) string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, getenv)
}

// Parse decodes and validates config JSON, fills in defaults and resolves
// secrets. It reports every problem it finds, not just the first.
func Parse(data []byte, getenv func(string) string) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a typo in a field name is an error, not a silent default
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(getenv); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Interval.Duration == 0 {
		c.Interval.Duration = DefaultInterval
	}
	if c.ProbeTimeout.Duration == 0 {
		c.ProbeTimeout.Duration = DefaultProbeTimeout
	}
	if c.FailThreshold == 0 {
		c.FailThreshold = DefaultFailThreshold
	}
	if c.RealertCooldown.Duration == 0 {
		c.RealertCooldown.Duration = DefaultRealertCooldown
	}
	for i := range c.Probes {
		p := &c.Probes[i]
		if p.Type == TypeHTTP && p.WarnCertDays == nil {
			days := DefaultWarnCertDays
			p.WarnCertDays = &days
		}
	}
}

func (c *Config) validate(getenv func(string) string) error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Instance == "" {
		add("instance is required")
	}
	if c.Interval.Duration < 0 {
		add("interval must be positive")
	}
	if c.ProbeTimeout.Duration < 0 {
		add("probe_timeout must be positive")
	}
	if c.FailThreshold < 0 {
		add("fail_threshold must be at least 1")
	}
	if c.RealertCooldown.Duration < 0 {
		add("realert_cooldown must be positive")
	}
	if len(c.Probes) == 0 {
		add("at least one probe is required")
	}

	// Probe names and peer names share one namespace: each becomes a
	// separately tracked check with its own alerts.
	names := map[string]bool{}
	claim := func(name string) {
		if name == "" {
			return
		}
		if names[name] {
			add("duplicate name %q (probe and peer names must be unique)", name)
		}
		names[name] = true
	}

	for i := range c.Probes {
		p := &c.Probes[i]
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
			add("probe %s: name is required", label)
		}
		claim(p.Name)
		for _, peer := range p.Peers {
			claim(peer.Name)
		}
		errs = append(errs, validateProbe(label, p, getenv)...)
	}
	return errors.Join(errs...)
}

func validateProbe(label string, p *Probe, getenv func(string) string) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("probe %s: "+format, append([]any{label}, args...)...))
	}

	switch p.Type {
	case TypeHTTP:
		if p.URL == "" {
			add("url is required")
		} else if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("url must be an absolute http:// or https:// URL")
		}
		// A bare 200 never proves the right app answered.
		if p.Expect == "" {
			add("expect is required (a piece of text the page must contain)")
		}
		if p.WarnCertDays != nil && *p.WarnCertDays < 0 {
			add("warn_cert_days must not be negative")
		}
		if p.Timeout.Duration < 0 {
			add("timeout must be positive")
		}

	case TypeHeartbeat:
		if p.Listen == "" {
			add("listen is required (e.g. \":8080\")")
		} else if _, _, err := net.SplitHostPort(p.Listen); err != nil {
			add("listen must be host:port or :port")
		}
		if token, err := resolveSecret(p.TokenEnv, getenv); err != nil {
			add("%v", err)
		} else {
			p.Token = token
		}
		for j, peer := range p.Peers {
			if peer.Name == "" {
				add("peer #%d: name is required", j+1)
			}
			if peer.MaxSilence.Duration <= 0 {
				add("peer %q: max_silence is required and must be positive", peer.Name)
			}
		}

	case "":
		add("type is required")
	default:
		add("unknown type %q (supported: %s, %s)", p.Type, TypeHTTP, TypeHeartbeat)
	}
	return errs
}

// resolveSecret reads the secret named by envName. Errors name the variable
// but never print a value. If envName does not look like a variable name it
// is not echoed either: someone may have pasted the secret itself there.
func resolveSecret(envName string, getenv func(string) string) (string, error) {
	if envName == "" {
		return "", errors.New("token_env is required (the NAME of the environment variable holding the secret)")
	}
	if !isEnvName(envName) {
		return "", errors.New("token_env must be an environment variable NAME (letters, digits, underscore), not a value")
	}
	value := getenv(envName)
	if value == "" {
		return "", fmt.Errorf("environment variable %s (token_env) is not set", envName)
	}
	return value, nil
}

func isEnvName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

// Duration is a time.Duration written in JSON as a Go duration string,
// such as "60s", "15m" or "6h".
type Duration struct {
	time.Duration
}

// UnmarshalJSON parses a duration string such as "90s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"60s\", got %s", b)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = parsed
	return nil
}
