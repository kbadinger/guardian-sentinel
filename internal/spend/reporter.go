package spend

import (
	"context"
	"log"
	"time"
)

// Sender delivers a report (alert.Telegram or alert.Printer).
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Reporter sends one spend report a day at Hour:Minute in Location.
type Reporter struct {
	Instance  string
	Sources   []Source
	Untracked []string
	Hour      int
	Minute    int
	Location  *time.Location
	Sender    Sender
	Now       func() time.Time // time.Now when nil
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SendOnce builds and sends one report as of now.
func (r *Reporter) SendOnce(ctx context.Context) error {
	now := r.now()
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lines := Collect(fetchCtx, r.Sources, now)
	day := now.UTC().AddDate(0, 0, -1)
	sendCtx, cancelSend := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSend()
	return r.Sender.Send(sendCtx, Format(r.Instance, day, lines, r.Untracked))
}

// Run sends a report at each scheduled time until ctx ends. Nothing is
// sent at startup: every deploy restarts the pod.
func (r *Reporter) Run(ctx context.Context) {
	for {
		next := NextRun(r.now(), r.Hour, r.Minute, r.Location)
		log.Printf("spend report: next at %s", next.Format("Mon Jan 2 15:04 MST"))
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := r.SendOnce(ctx); err != nil {
			log.Printf("spend report: send failed: %v", err)
		}
	}
}
