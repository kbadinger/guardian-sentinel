// Command guardian-sentinel is a small site monitor. It reads a JSON config
// (path in SENTINEL_CONFIG), checks every probe once per interval and sends
// alerts to Telegram (SENTINEL_DRY_RUN=1 prints them instead).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the scratch image and Windows ship no zoneinfo; alerts are stamped in ET

	"github.com/kbadinger/guardian-sentinel/internal/alert"
	"github.com/kbadinger/guardian-sentinel/internal/config"
	"github.com/kbadinger/guardian-sentinel/internal/monitor"
	"github.com/kbadinger/guardian-sentinel/internal/probe"
	"github.com/kbadinger/guardian-sentinel/internal/spend"
)

// shutdownTimeout bounds the drain on SIGTERM. Kubernetes waits 30s by
// default before it kills the pod.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Printf("guardian-sentinel: %v", err)
		os.Exit(1)
	}
}

func run() error {
	path := os.Getenv("SENTINEL_CONFIG")
	if path == "" {
		return errors.New("SENTINEL_CONFIG is not set (it must be the path to the JSON config file)")
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		return err
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return fmt.Errorf("load time zone: %w", err)
	}

	var sender monitor.Sender
	if os.Getenv("SENTINEL_DRY_RUN") == "1" {
		log.Printf("dry run: alerts are printed, not sent")
		sender = &alert.Printer{W: os.Stdout}
	} else {
		tg, err := alert.NewTelegramFromEnv(os.Getenv)
		if err != nil {
			return err
		}
		sender = tg
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	var reporter *spend.Reporter
	if cfg.Spend != nil {
		reporter = newSpendReporter(cfg, ny, sender)
	}
	// SENTINEL_SPEND_ONCE=1 sends one spend report now and exits: a manual
	// check that the keys and the message work.
	if os.Getenv("SENTINEL_SPEND_ONCE") == "1" {
		if reporter == nil {
			return errors.New("SENTINEL_SPEND_ONCE is set but the config has no spend block")
		}
		return reporter.SendOnce(ctx)
	}

	// The heartbeat receiver reports accepted heartbeats to the runner, and
	// the runner polls the receiver's peers, so the runner is created after
	// the checks and the callback reaches it through this variable. Ports
	// are bound in the loop (a port in use is a startup error) but only
	// served once runner is set.
	var runner *monitor.Runner
	report := func(peer string, r probe.Result) { runner.Report(peer, r) }

	var checks []monitor.Check
	var listeners []listener
	httpClient := probe.NewHTTPClient()
	for _, p := range cfg.Probes {
		switch p.Type {
		case config.TypeHTTP:
			timeout := cfg.ProbeTimeout.Duration
			if p.Timeout.Duration > 0 {
				timeout = p.Timeout.Duration
			}
			checks = append(checks, monitor.Check{
				Name:    p.Name,
				Prober:  &probe.HTTP{URL: p.URL, Expect: p.Expect, WarnCertDays: *p.WarnCertDays, Client: httpClient},
				Timeout: timeout,
			})

		case config.TypeHeartbeat:
			var peers []probe.Peer
			for _, peer := range p.Peers {
				peers = append(peers, probe.Peer{Name: peer.Name, MaxSilence: peer.MaxSilence.Duration})
			}
			recv := probe.NewReceiver([]byte(p.Token), peers, nil, report)
			for _, peer := range peers {
				// Silence is already measured by max_silence, so the first failed
				// check is DOWN.
				checks = append(checks, monitor.Check{
					Name: peer.Name, Prober: recv.PeerCheck(peer.Name),
					Timeout: cfg.ProbeTimeout.Duration, Threshold: 1, Peer: true,
				})
			}
			ln, err := net.Listen("tcp", p.Listen)
			if err != nil {
				return fmt.Errorf("probe %s: %w", p.Name, err)
			}
			listeners = append(listeners, listener{ln, recv})

		default:
			// config.Load rejects unknown types; this guards a type added to
			// config but not wired here.
			return fmt.Errorf("probe %s: type %q is not wired up", p.Name, p.Type)
		}
	}

	runner = monitor.NewRunner(monitor.Settings{
		Instance:      cfg.Instance,
		FailThreshold: cfg.FailThreshold,
		Cooldown:      cfg.RealertCooldown.Duration,
		Location:      ny,
	}, cfg.Interval.Duration, checks, sender)

	var servers []*http.Server
	for _, l := range listeners {
		l.recv.SetHealthCheck(func() bool { return runner.Healthy(time.Now()) })
		servers = append(servers, serve(l))
	}

	log.Printf("sentinel[%s] starting: %d checks every %s", cfg.Instance, len(checks), cfg.Interval.Duration)
	reportDone := make(chan struct{})
	if reporter != nil {
		go func() { defer close(reportDone); reporter.Run(ctx) }()
	} else {
		close(reportDone)
	}
	runner.Run(ctx) // returns on SIGTERM / Ctrl-C
	<-reportDone

	// Stop taking heartbeats first, then let alerts already due go out.
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}
	runner.Wait()
	return nil
}

// newSpendReporter wires the config's spend providers to their readers.
func newSpendReporter(cfg *config.Config, loc *time.Location, sender monitor.Sender) *spend.Reporter {
	r := &spend.Reporter{
		Instance: cfg.Instance, Hour: cfg.Spend.Hour, Minute: cfg.Spend.Minute,
		Location: loc, Sender: sender, Untracked: cfg.Spend.Untracked,
	}
	client := &http.Client{Timeout: time.Minute}
	for _, p := range cfg.Spend.Providers {
		switch p.Type {
		case config.SpendAnthropic:
			r.Sources = append(r.Sources, &spend.Anthropic{Label: p.Name, Key: p.Key, Client: client})
		case config.SpendVercel:
			r.Sources = append(r.Sources, &spend.Vercel{Label: p.Name, Key: p.Key, TeamID: p.TeamID, Client: client})
		}
	}
	return r
}

// listener is a bound port waiting to serve a heartbeat receiver.
type listener struct {
	ln   net.Listener
	recv *probe.Receiver
}

// serve runs an HTTP server on l in the background. The timeouts matter:
// the heartbeat port is reachable from the internet.
func serve(l listener) *http.Server {
	srv := &http.Server{
		Handler:           l.recv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.Serve(l.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server on %s stopped: %v", l.ln.Addr(), err)
		}
	}()
	log.Printf("heartbeat receiver listening on %s (POST /heartbeat, GET /healthz)", l.ln.Addr())
	return srv
}
