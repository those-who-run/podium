package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

	"github.com/those-who-run/podium/internal/card"
)

// ended is a recording's session.ended events, as internal/card's tests keep them.
func ended(name string) string {
	return filepath.Join("..", "..", "internal", "card", "testdata", "ended", name+".jsonl")
}

func runCard(t *testing.T, args []string, vars map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(t.Context(), append([]string{"card"}, args...), env(vars), &out, &errs)
	return code, out.String(), errs.String()
}

// R4: the card for a stored session.ended, with the pit wall link, as v1's
// `post --dry-run` printed it.
func TestCardPrintsTheCardWithTheLink(t *testing.T) {
	code, out, errs := runCard(t, []string{ended("real-nuc-fuji-timed-2026-10-15")},
		map[string]string{"TWR_PITWALL_URL": "http://pitwall.home.arpa/"})
	want := "Fuji International Speedway · time trial\n" +
		"http://pitwall.home.arpa/?session=01KDVDNA000ZB038PDRJACDC76\n" +
		"2 laps\n  Best lap: 2:00.314 (lap 2)\n  Car: NISMO 400R '95\n"
	if code != 0 || out != want {
		t.Errorf("exit %d, stderr %q, printed\n%s\nwant\n%s", code, errs, out, want)
	}
	// --pitwall wins over the environment, and empty is no link.
	_, out, _ = runCard(t, []string{"--pitwall", "", ended("real-nuc-fuji-timed-2026-10-15")},
		map[string]string{"TWR_PITWALL_URL": "http://pitwall.home.arpa/"})
	if strings.Contains(out, "?session=") {
		t.Errorf("a link with --pitwall empty:\n%s", out)
	}
}

func TestCardSaysWhichSessionGetsNone(t *testing.T) {
	code, out, _ := runCard(t, []string{ended("real-nuc-sugo-restarts-2026-10-18")}, nil)
	if code != 0 || strings.Count(out, "Sportsland SUGO · 2-lap race\n") != 2 ||
		!strings.Contains(out, ": no full lap: nothing to post\n") {
		t.Errorf("exit %d, printed\n%s", code, out)
	}
}

func TestCardReadsStandardInput(t *testing.T) {
	data, err := os.ReadFile(ended("real-nuc-sugo-restarts-2026-10-18"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	stdin = strings.NewReader(lines[1]) // the restart on lap 1
	t.Cleanup(func() { stdin = os.Stdin })
	code, out, _ := runCard(t, []string{"-"}, nil)
	if code != 1 || !strings.HasSuffix(out, ": no full lap: nothing to post\n") {
		t.Errorf("exit %d, printed %q: want 1 and no card", code, out)
	}
}

func TestCardFailures(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jsonl")
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(`{"sessionId": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{nil, 2, "usage: podium card"},
		{[]string{"a", "b"}, 2, "usage: podium card"},
		{[]string{"--nope", "a"}, 2, "-nope"},
		{[]string{"-h"}, 0, "-pitwall"},
		{[]string{filepath.Join(dir, "missing")}, 1, "no such file"},
		{[]string{empty}, 1, "no session.ended in it"},
		{[]string{bad}, 1, "not a session.ended"},
		{[]string{"--sid", "s", "a"}, 2, "usage: podium card"},
		{[]string{"--sid", "s"}, 2, "set TWR_NATS_URL"},
	} {
		code, _, errs := runCard(t, tc.args, nil)
		if code != tc.code || !strings.Contains(errs, tc.says) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.args, code, errs, tc.code, tc.says)
		}
	}
}

// paddock#66, R4: --sid reads the session's session.ended from the EVENTS
// stream (internal/cards' TestReadEnded reads one from a real server).
func TestCardReadsASessionFromTheStream(t *testing.T) {
	f, err := os.Open(ended("real-nuc-fuji-timed-2026-10-15"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	stored, err := card.ReadEnded(f)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	readBySID = func(_ context.Context, url, src, sid string) (*eventsv1.SessionEnded, error) {
		asked = append(asked, url, src, sid)
		if sid == "missing" {
			return nil, errors.New("no session.ended in EVENTS")
		}
		return stored[0], nil
	}
	t.Cleanup(func() { readBySID = readEvents })
	vars := map[string]string{"TWR_NATS_URL": "nats://n:4222", "TWR_PODIUM_SRC": "replay-x"}
	code, out, errs := runCard(t, []string{"--sid", "01KDVDNA000ZB038PDRJACDC76"}, vars)
	if code != 0 || !strings.HasPrefix(out, "Fuji International Speedway · time trial\n") ||
		strings.Join(asked, " ") != "nats://n:4222 replay-x 01KDVDNA000ZB038PDRJACDC76" {
		t.Errorf("exit %d, stderr %q, asked %v, printed\n%s", code, errs, asked, out)
	}
	asked = nil
	code, _, errs = runCard(t, []string{"--src", "live", "--sid", "missing"}, vars)
	if code != 1 || !strings.Contains(errs, "no session.ended") || asked[1] != "live" {
		t.Errorf("a missing session: exit %d, stderr %q, asked %v", code, errs, asked)
	}
}

// The real reader, at a NATS server that isn't there, says so.
func TestReadBySIDWithNoServer(t *testing.T) {
	if _, err := readEvents(t.Context(), "nats://127.0.0.1:1", "live", "s"); err == nil {
		t.Error("no error with no server")
	}
}
