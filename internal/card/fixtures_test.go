package card

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The cards are compared with what v1 1.0.0 posted for the same recordings
// (paddock#66, AC1 to AC4). testdata/ended holds each recording's session.ended
// events as ecu published them, at v0.1.11 or the later tag make_ended.go.txt
// names for the recording (testdata/ended/make_ended.go.txt says how);
// testdata/v1 holds v1's `post --dry-run` of each session v1 let go of, in
// order, with what the dry run doesn't print after it (testdata/v1/make_cards.py
// says how). Both were made once and are kept.

// pitwall is the pit wall base URL v1's cards were made with.
const pitwall = "http://pitwall.test/"

// v1Card is one card v1 printed: its dry-run text, and the colour, footer and
// start time (in Unix seconds) it carries beside it. none is v1's "no full lap:
// nothing to post".
type v1Card struct {
	text, footer string
	colour       int
	started      float64
	none         bool
}

var extra = regexp.MustCompile(`^-- colour=0x([0-9A-F]{6}) footer=(.*) timestamp=([0-9.]+)$`)

func v1Cards(t *testing.T, name string) []v1Card {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1", name+".txt")) //nolint:gosec // a fixture of the tests
	if err != nil {
		t.Fatal(err)
	}
	var out []v1Card
	for block := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n\n") {
		if block == "no full lap: nothing to post" {
			out = append(out, v1Card{none: true})
			continue
		}
		lines := strings.Split(block, "\n")
		m := extra.FindStringSubmatch(lines[len(lines)-1])
		if m == nil {
			t.Fatalf("%s: a card without its colour line: %q", name, block)
		}
		colour, _ := strconv.ParseInt(m[1], 16, 32)
		started, _ := strconv.ParseFloat(m[3], 64)
		out = append(out, v1Card{
			text:   strings.Join(lines[:len(lines)-1], "\n") + "\n",
			colour: int(colour), footer: m[2], started: started,
		})
	}
	return out
}

func endedOf(t *testing.T, name string) []*eventsv1.SessionEnded {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "ended", name+".jsonl")) //nolint:gosec // a fixture of the tests
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ended, err := ReadEnded(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return ended
}

// v1Link is the link line of a v1 card: v1's session ids are its start times,
// v2's are ULIDs, so the link is compared with the session's own id in it.
var v1Link = regexp.MustCompile(`(?m)^(http://pitwall\.test/\?session=)\S+$`)

// accepted are the known differences from v1's cards, each with its reason and
// the decision behind it. v1's files stay as v1 made them; the line named here is
// taken out of v1's card before the comparison, and the test fails if v1's card
// no longer has it or v2's card has it too; heldMs is how much later v1 dated
// the card than v2, and the test fails unless it is exactly that, so an entry
// never outlives its difference.
var accepted = map[string]map[int]struct {
	line   string
	heldMs int64
	why    string
}{
	"real-quali-restart-quit-2026-10-14": {
		1: {
			line: "  Best lap: 8:26.429 (lap 1)\n",
			why: "the card shows ecu's best lap (the maintainer's decision, 2026-10-30): ecu leaves out laps " +
				"that don't count for best, here lap 1, the only one timed, so the session has none",
		},
	},
	"real-race-2laps-2026-10-14": {
		1: {
			heldMs: 3001,
			why: "a held start (cars-on-track off on its first frame) starts at its first held frame " +
				"(contracts v0.2.3, paddock#74), where v1's clock read the start check's verdict, 3.001 s later",
		},
	},
}

// sameAsV1 checks each session's card against v1's, field for field: the
// title, the link (with v2's session id), the result, the fields in order,
// the colour, the footer and the timestamp (to the millisecond: v1 stored
// seconds as a float).
func sameAsV1(t *testing.T, name string) []Card {
	t.Helper()
	ended, want := endedOf(t, name), v1Cards(t, name)
	if len(ended) != len(want) {
		t.Fatalf("%s: %d sessions ended, v1 let go of %d", name, len(ended), len(want))
	}
	var cards []Card
	for i, e := range ended {
		got, ok := Build(e, pitwall)
		w := want[i]
		if w.none {
			if ok {
				t.Errorf("%s session %d: a card %q, v1 posted none", name, i+1, got.Title)
			}
			continue
		}
		if !ok {
			t.Errorf("%s session %d: no card, v1 posted:\n%s", name, i+1, w.text)
			continue
		}
		cards = append(cards, got)
		wantText := v1Link.ReplaceAllString(w.text, "${1}"+e.GetSessionId())
		ms := int64(math.Round(float64(got.Timestamp.UnixNano()) / 1e6))
		wantMs := int64(math.Round(w.started * 1000))
		diff, ok := accepted[name][i+1]
		if ok && diff.line == "" && diff.heldMs == 0 {
			t.Fatalf("%s session %d: an accepted difference with neither a line nor heldMs (%s)", name, i+1, diff.why)
		}
		if ok && diff.heldMs != 0 {
			if wantMs-ms != diff.heldMs {
				t.Errorf("%s session %d: v1 started %d ms after v2, not the accepted %d ms (%s): change or remove it",
					name, i+1, wantMs-ms, diff.heldMs, diff.why)
			}
			wantMs -= diff.heldMs
		}
		if ok && diff.line != "" {
			if !strings.Contains(wantText, diff.line) || strings.Contains(got.Text(), diff.line) {
				t.Errorf("%s session %d: the accepted difference %q (%s) is gone: remove it",
					name, i+1, diff.line, diff.why)
			}
			wantText = strings.Replace(wantText, diff.line, "", 1)
		}
		if got.Text() != wantText {
			t.Errorf("%s session %d:\n got: %s\nwant: %s", name, i+1, got.Text(), wantText)
		}
		if got.Colour != w.colour || got.Footer != w.footer || ms != wantMs {
			t.Errorf("%s session %d: colour %06X, footer %q, started %d ms; v1 %06X, %q, %.3f s",
				name, i+1, got.Colour, got.Footer, ms, w.colour, w.footer, w.started)
		}
	}
	return cards
}

// AC1: the finished race, the two stints, the time trial with the pit wall
// link, the timed finish, the restart then quit, and a practice then a race on
// one bus (paddock#356) give v1's cards, but for the accepted differences.
func TestTheCardsAreV1s(t *testing.T) {
	for _, name := range []string{
		"real-nuc-suzuka-no-stop-2026-10-15",
		"real-nuc-suzuka-pit-stop-2026-10-15",
		"real-nuc-fuji-timed-2026-10-15",
		"real-nuc-suzuka-timed-race-2026-10-16",
		"real-quali-restart-quit-2026-10-14",
		"real-race-2laps-2026-10-14",
	} {
		t.Run(name, func(t *testing.T) { sameAsV1(t, name) })
	}
}

// AC1: the time trial's title links to the session on the pit wall.
func TestTheTimeTrialLinksToThePitWall(t *testing.T) {
	cards := sameAsV1(t, "real-nuc-fuji-timed-2026-10-15")
	ended := endedOf(t, "real-nuc-fuji-timed-2026-10-15")
	if want := "http://pitwall.test/?session=" + ended[0].GetSessionId(); cards[0].URL != want {
		t.Errorf("link %q, want %q", cards[0].URL, want)
	}
}

// AC2: Spa's quit is a timed race at Circuit de Spa-Francorchamps.
func TestSpasTitle(t *testing.T) {
	cards := sameAsV1(t, "real-nuc-spa-pit-cut-quit-2026-10-18")
	if want := "Circuit de Spa-Francorchamps · timed race"; cards[0].Title != want {
		t.Errorf("title %q, want %q", cards[0].Title, want)
	}
}

// AC3: Suzuka before the restart, then joined mid-race on the same bus, is one
// session and one card, which says the engineer restarted on lap 7.
func TestTheRestartPairIsOneCard(t *testing.T) {
	cards := sameAsV1(t, "restart-pair")
	if len(cards) != 1 {
		t.Fatalf("%d cards, want 1", len(cards))
	}
	if !hasField(cards[0], "Engineer restarted", "on lap 7") {
		t.Errorf("fields %v, want Engineer restarted on lap 7", cards[0].Fields)
	}
}

// AC4: SUGO's restart on lap 1 gets no card; the races either side of it do.
func TestSugosLap1RestartGetsNoCard(t *testing.T) {
	ended := endedOf(t, "real-nuc-sugo-restarts-2026-10-18")
	if len(ended) != 3 {
		t.Fatalf("%d sessions, want 3", len(ended))
	}
	var got []bool
	for _, e := range ended {
		_, ok := Build(e, "")
		got = append(got, ok)
	}
	if fmt.Sprint(got) != "[true false true]" {
		t.Errorf("cards %v, want [true false true]", got)
	}
	sameAsV1(t, "real-nuc-sugo-restarts-2026-10-18")
}

func hasField(c Card, name, value string) bool {
	for _, f := range c.Fields {
		if f.Name == name && f.Value == value {
			return true
		}
	}
	return false
}

// A ranked session (contracts v0.2.7, the maintainer's decision of 2026-10-31):
// the maintainer's qualifying at Suzuka on 2026-10-31 evening, two laps and back
// to the pits, P7 of 16, which v1 called a timed race. Its session.ended is ecu's
// (testdata/ended/real-nuc-suzuka-qualifying-2026-10-31.jsonl, made as
// make_ended.go.txt says, from ecu's ranked kind branch on its fixture).
func TestARankedSessionsCard(t *testing.T) {
	ended := endedOf(t, "real-nuc-suzuka-qualifying-2026-10-31")
	if len(ended) != 1 || ended[0].GetSummary().GetSessionType() != eventsv1.SessionType_SESSION_TYPE_RANKED {
		t.Fatalf("want one ranked session, got %v", ended)
	}
	c, ok := Build(ended[0], pitwall)
	if !ok {
		t.Fatal("no card")
	}
	if c.Title != "Suzuka Circuit · qualifying or practice" {
		t.Errorf("title %q", c.Title)
	}
	if want := "2 laps, P7 of 16"; c.Description != want && !strings.HasPrefix(c.Description, want) {
		t.Errorf("result %q, want %q", c.Description, want)
	}
}
