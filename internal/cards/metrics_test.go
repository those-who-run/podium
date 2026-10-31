package cards

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("gone") }

// A nil *Counters counts nothing and writes zeroes; a writer that fails is
// reported.
func TestCounters(t *testing.T) {
	var none *Counters
	none.add(OK)
	var b strings.Builder
	if _, err := none.WriteTo(&b); err != nil || none.Get(OK) != 0 || !strings.Contains(b.String(), `{result="ok"} 0`) {
		t.Errorf("a nil Counters: %v\n%s", err, b.String())
	}
	if _, err := (&Counters{}).WriteTo(failingWriter{}); err == nil {
		t.Error("a failing writer is no error")
	}
}

// The real wait between attempts ends with its time, or sooner with ctx.
func TestPause(t *testing.T) {
	p := &Poster{}
	if err := p.pause(t.Context(), time.Millisecond); err != nil {
		t.Errorf("pause = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.pause(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled pause = %v", err)
	}
	if p.clock().IsZero() {
		t.Error("no clock")
	}
}
