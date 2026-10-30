// Package sink is where podium puts a results card: Discord's results channel
// (internal/discord), or the console, which needs no Discord, for replays, tests
// and `podium card` (v1's `post --dry-run`), as radio's console sink is for
// callouts.
package sink

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/those-who-run/podium/internal/card"
)

// Sink posts a session's results card. sid is the session's id, for the logs
// and for a sink that keeps a record of what it posted. A Sink does no retries:
// its caller decides what to do with an error.
type Sink interface {
	Post(ctx context.Context, sid string, c card.Card) error
}

// Console writes each card as v1's `post --dry-run` printed it, with a blank line
// between cards. It is safe for concurrent use.
type Console struct {
	mu sync.Mutex
	w  io.Writer
	n  int
}

// NewConsole is a Console writing to w.
func NewConsole(w io.Writer) *Console {
	return &Console{w: w}
}

// Post writes c.
func (s *Console) Post(_ context.Context, _ string, c card.Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := c.Text()
	if s.n > 0 {
		text = "\n" + text
	}
	s.n++
	if _, err := io.WriteString(s.w, text); err != nil {
		return fmt.Errorf("console: %w", err)
	}
	return nil
}
