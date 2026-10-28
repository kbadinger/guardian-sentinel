package alert

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Printer writes alerts to W instead of sending them (SENTINEL_DRY_RUN=1).
type Printer struct {
	mu sync.Mutex
	W  io.Writer
}

// Send writes one line: "[dry-run] <text>".
func (p *Printer) Send(_ context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := fmt.Fprintf(p.W, "[dry-run] %s\n", text)
	return err
}
