package cards

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Result is how a card ended, the result label of gt7_results_posts_total.
type Result int

// The results, as v1 counted them (R5).
const (
	// OK is a card posted.
	OK Result = iota
	// Failed is a card given up on: every attempt failed, a refusal no retry
	// can fix, or a card a stopped poster may have posted that can't be
	// checked.
	Failed
	// Skipped is a session that gets no card: no full lap.
	Skipped
)

var resultNames = [...]string{OK: "ok", Failed: "failed", Skipped: "skipped"}

// Counters is gt7_results_posts_total{result}, v1's series under its v1 name
// (paddock#67's triage: v1's series keep their names). It is safe for
// concurrent use; a nil *Counters counts nothing.
type Counters struct {
	n [len(resultNames)]atomic.Uint64
}

func (c *Counters) add(r Result) {
	if c != nil {
		c.n[r].Add(1)
	}
}

// Get is the count for r.
func (c *Counters) Get(r Result) uint64 {
	if c == nil {
		return 0
	}
	return c.n[r].Load()
}

// WriteTo writes the counter in Prometheus's text format, each result with a
// line of its own, 0 included, so a rate over it starts at the first scrape.
func (c *Counters) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprint(w, "# HELP gt7_results_posts_total Results cards: posted (ok), given up on (failed), "+
		"or not posted for a session with no full lap (skipped).\n# TYPE gt7_results_posts_total counter\n")
	total := int64(n)
	for r, name := range resultNames {
		if err != nil {
			break
		}
		n, err = fmt.Fprintf(w, "gt7_results_posts_total{result=%q} %d\n", name, c.Get(Result(r)))
		total += int64(n)
	}
	return total, err
}
