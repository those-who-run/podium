package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		code, _, errs := runCard(t, tc.args, nil)
		if code != tc.code || !strings.Contains(errs, tc.says) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.args, code, errs, tc.code, tc.says)
		}
	}
}
