// Package card builds a session's results card from its session.ended event
// (paddock#66): v1 1.0.0's results.build_card, field for field, so a card reads
// as the season of cards v1 posted before it. Build is pure: no I/O, no clock.
//
// Its title and result, and the times and fuel figures, are written by
// contracts' format package, which the pit wall shares, so the card and the
// page never show the same session or lap differently (paddock#128).
//
// Where v1 read summary.json, the card reads SessionEnded's summary, which
// carries the same fields (contracts' SessionSummary names the v1 field each
// one carries). One departure is the maintainer's decision (2026-10-30): the
// best lap is ecu's, the summary's best_lap and best_lap_time_ms, so the card
// and the pit wall always agree (ecu is the brain), where v1's card worked out
// its own (results.best_lap). They differ where no lap counts for ecu's best
// and v1 took one anyway: ecu leaves out pit laps and every lap that doesn't
// count (contracts' SessionSummary.best_lap), so the first qualifying of
// real-quali-restart-quit-2026-10-14, whose only timed lap is lap 1, shows no
// best lap, where v1 showed "8:26.429 (lap 1)".
//
// Two of v1's fields have none of that name:
//   - v1's `engineer_shutdown` is SessionEnded's reason
//     END_REASON_ENGINEER_STOPPED: ecu publishes session.ended once, at the
//     session's real end, so the card holds nothing back (#66 R3);
//   - v1's `type`, its first-frame guess of race, time trial or practice, has
//     no v2 field (the summary's session_type is ecu's final kind), so a
//     session whose kind ecu could not tell is a "session", where v1 could say
//     "race" or "time trial".
package card

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/those-who-run/contracts/format"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// The brand, copied from v1's brand.py (paddock#66: the three constants the card
// uses, with the name and kanji its footer writes). The design language is
// paddock ADR 0004's; pitwall's brand.css is edited by hand (paddock#129).
const (
	// AdvanRed is a finished session's colour: v1's brand.ADVAN_RED.
	AdvanRed = 0xD21F1B
	// CarbonGrey is every other session's colour: v1's brand.CARBON_GREY.
	CarbonGrey = 0x333333
	// GlyphFlag comes before a finished result: v1's brand.GLYPH_FLAG, as
	// contracts' format writes it.
	GlyphFlag = format.GlyphFlag
	// Name and Kanji are the footer: v1's brand.NAME and brand.KANJI.
	Name  = "Those Who Run"
	Kanji = "走り屋"
)

// Card is a results card: what Discord shows as an embed. It is v1's
// results.Card.
type Card struct {
	// Title is "<track> · <kind>", such as "Sportsland SUGO · 5-lap race".
	Title string
	// Description is the result, such as "🚩 Finished P3 of 20 in 8:14.302".
	Description string
	// Fields are the card's fields, in order, each shown inline.
	Fields []Field
	// Colour is AdvanRed for a finished session, CarbonGrey otherwise.
	Colour int
	// Footer is the product's name and kanji.
	Footer string
	// Timestamp is when the session started; the zero time when it has none.
	// Discord shows it in each viewer's own time.
	Timestamp time.Time
	// URL is the session's page on the pit wall, which the title links to;
	// empty without a pit wall base URL.
	URL string
}

// Field is one of a card's fields: a name and its value.
type Field struct {
	Name, Value string
}

// Build is the card for a session, from its session.ended, and false when the
// session gets none: no full lap was completed (v1: laps_completed < 1). With a
// pit wall base URL, the title links to the session's page on it.
func Build(ended *eventsv1.SessionEnded, pitwallURL string) (Card, bool) {
	s := ended.GetSummary()
	laps := int(s.GetLapsCompleted())
	if laps < 1 {
		return Card{}, false
	}
	kind := s.GetKind()
	finished := s.GetFinished()
	var fields []Field
	// ecu's best lap, which the pit wall shows too; none when ecu decided none.
	if s.BestLap != nil && s.GetBestLapTimeMs() > 0 {
		fields = append(fields, Field{"Best lap",
			fmt.Sprintf("%s (lap %d)", format.RaceTime(int(s.GetBestLapTimeMs())), s.GetBestLap())})
	}
	if car := s.GetCarName(); car != "" {
		fields = append(fields, Field{"Car", car})
	}
	if joined := s.GetJoinedOnLap(); joined > 0 {
		// ecu started part way through: the laps before it are not here.
		fields = append(fields, Field{"Joined", fmt.Sprintf("on lap %d", joined)})
	}
	var restarts []string
	for _, g := range s.GetGaps() {
		if g.GetLap() > 0 {
			restarts = append(restarts, strconv.Itoa(int(g.GetLap())))
		}
	}
	if len(restarts) > 0 {
		// Continued across an ecu restart: a lap of it was not all seen.
		word := "lap"
		if len(restarts) > 1 {
			word = "laps"
		}
		fields = append(fields, Field{"Engineer restarted", "on " + word + " " + strings.Join(restarts, ", ")})
	}
	used, capacity := s.GetFuelBurned(), s.GetFuelCapacity()
	if kind.GetFuelUse() == eventsv1.FuelUse_FUEL_USE_ON && used > 0 && capacity > 0 {
		// One figure per stint, as the engineer says it: never more than a tank.
		stints := s.GetFuelBurnedByStint()
		if len(stints) == 0 {
			stints = []float64{used}
		}
		pcts := make([]string, len(stints))
		for i, x := range stints {
			pcts[i] = format.FuelPct(100*x/capacity, false)
		}
		fields = append(fields, Field{"Fuel used", strings.Join(pcts, ", then ")})
	}

	c := Card{
		Title:       format.Title(s),
		Description: format.Result(s, ended.GetReason(), true, 0),
		Fields:      fields,
		Colour:      CarbonGrey,
		Footer:      Name + " " + Kanji,
		URL:         SessionLink(pitwallURL, ended.GetSessionId()),
	}
	if finished {
		c.Colour = AdvanRed
	}
	if at := s.GetStartedAt(); at != nil && at.AsTime().Unix() != 0 {
		c.Timestamp = at.AsTime().UTC()
	}
	return c, true
}

// SessionLink is a session's page on the pit wall, "<base>/?session=<sid>", or
// "" without a base URL or a session id. The id is escaped as Python's
// urllib.parse.quote(sid, safe="") does, as v1 wrote it.
func SessionLink(base, sid string) string {
	if base == "" || sid == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/?session=" + quote(sid)
}

// quote escapes every byte but the unreserved ones (letters, digits, "_.-~").
func quote(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// Text is the card as v1's `those-who-run post --dry-run` printed it: the
// title, the link when there is one, the result, then each field indented as
// "  Name: value", each on a line of its own.
func (c Card) Text() string {
	var b strings.Builder
	b.WriteString(c.Title + "\n")
	if c.URL != "" {
		b.WriteString(c.URL + "\n")
	}
	b.WriteString(c.Description + "\n")
	for _, f := range c.Fields {
		fmt.Fprintf(&b, "  %s: %s\n", f.Name, f.Value)
	}
	return b.String()
}
