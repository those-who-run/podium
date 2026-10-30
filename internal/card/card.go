// Package card builds a session's results card from its session.ended event
// (paddock#66): v1 1.0.0's results.build_card, field for field, so a card reads
// as the season of cards v1 posted before it. Build is pure: no I/O, no clock.
//
// Times and fuel figures are written by contracts' format package, which the
// pit wall shares, so the card and the page never show the same lap
// differently (paddock#128).
//
// Where v1 read summary.json, the card reads SessionEnded's summary, which
// carries the same fields (contracts' SessionSummary names the v1 field each
// one carries). Two are read differently, as the summary has no field of v1's
// name for them:
//   - v1's `engineer_shutdown` is SessionEnded's reason
//     END_REASON_ENGINEER_STOPPED: ecu publishes session.ended once, at the
//     session's real end, so the card holds nothing back (#66 R3);
//   - v1's `type`, the label of a session whose kind is unknown, is the
//     summary's session_type.
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
	// GlyphFlag comes before a finished result: v1's brand.GLYPH_FLAG.
	GlyphFlag = "🚩"
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
	sessionType := eventsv1.SessionType_SESSION_TYPE_UNKNOWN
	if kind != nil {
		sessionType = kind.GetSessionType()
	}
	totalLaps := int(s.GetTotalLaps())
	position := 0
	if PositionShown(kind) {
		position = int(s.GetFinalPosition())
	}
	cars := int(s.GetTotalCars())
	finished := s.GetFinished()

	title := TrackLabel(s.GetTrack()) + " · " + KindLabel(sessionType, totalLaps, s.GetSessionType())

	ofCars := ""
	if cars > 0 {
		ofCars = " of " + strconv.Itoa(cars)
	}
	var result string
	switch {
	case finished:
		result = "Finished"
		if position > 0 {
			result = fmt.Sprintf("Finished P%d%s", position, ofCars)
		}
		if total := int(s.GetTotalTimeMs()); total > 0 {
			result += " in " + format.RaceTime(total)
		}
	case s.GetEndedPaused():
		result = PausedResult(s, position)
	case ended.GetReason() == eventsv1.EndReason_END_REASON_ENGINEER_STOPPED:
		result = StoppedResult(s, position)
	case sessionType == eventsv1.SessionType_SESSION_TYPE_LAP_RACE && totalLaps > 0:
		result = fmt.Sprintf("Retired on lap %d of %d", min(laps+1, totalLaps), totalLaps)
		if position > 0 {
			result += fmt.Sprintf(", running P%d", position)
		}
	default:
		result = plural(laps, "lap")
		if position > 0 {
			result += fmt.Sprintf(", P%d%s", position, ofCars)
		}
	}

	var fields []Field
	if lap, best, ok := BestLap(s.GetLaps(), sessionType == eventsv1.SessionType_SESSION_TYPE_LAP_RACE,
		int(s.GetGameBestLapMs())); ok {
		fields = append(fields, Field{"Best lap", fmt.Sprintf("%s (lap %d)", format.RaceTime(best), lap)})
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
		Title:       title,
		Description: result,
		Fields:      fields,
		Colour:      CarbonGrey,
		Footer:      Name + " " + Kanji,
		URL:         SessionLink(pitwallURL, ended.GetSessionId()),
	}
	if finished {
		c.Description = GlyphFlag + " " + result
		c.Colour = AdvanRed
	}
	if at := s.GetStartedAt(); at != nil && at.AsTime().Unix() != 0 {
		c.Timestamp = at.AsTime().UTC()
	}
	return c, true
}

// PausedResult is the result of a session that ended in a pause: "Paused on lap
// 1, P2", "Paused on lap 3 of 5". It is not a retirement: the race may still be
// paused on the PS5. position 0 is none shown.
func PausedResult(s *eventsv1.SessionSummary, position int) string {
	laps, total := int(s.GetLapsCompleted()), int(s.GetTotalLaps())
	lap := laps + 1
	result := ""
	if total > 0 {
		result = fmt.Sprintf("Paused on lap %d of %d", min(lap, total), total)
	} else {
		result = fmt.Sprintf("Paused on lap %d", lap)
	}
	return withPosition(result, position)
}

// StoppedResult is the result of a session ecu's stop ended and no restart
// continued: "Engineer stopped on lap 7, P1". It is not a retirement: the race
// went on. The lap is the last one seen. position 0 is none shown.
func StoppedResult(s *eventsv1.SessionSummary, position int) string {
	lap := int(s.GetLapsCompleted()) + 1
	if len(s.GetLaps()) > 0 {
		lap = 0
		for _, l := range s.GetLaps() {
			lap = max(lap, int(l.GetLap()))
		}
	}
	result := fmt.Sprintf("Engineer stopped on lap %d", lap)
	if total := int(s.GetTotalLaps()); total > 0 {
		result += fmt.Sprintf(" of %d", total)
	}
	return withPosition(result, position)
}

func withPosition(result string, position int) string {
	if position > 0 {
		return fmt.Sprintf("%s, P%d", result, position)
	}
	return result
}

// PositionShown says whether a position means anything: a field GT7 flags, as
// the engineer says it.
func PositionShown(kind *eventsv1.SessionKind) bool {
	return kind.GetField() && kind.GetCarsOnTrack()
}

// BestLap is the lap number and time of the session's quickest lap, and false
// when it has none. GT7's own best (gameBest, 0 for none) wins when a lap has its
// time: it leaves out a start lap from a grid ahead of the line. Without it, the
// quickest complete lap, skipping lap 1 of a lap race (skipLap1) when another lap
// is timed, as the spoken summary does.
//
// It is v1's results.best_lap, which the card used, not the summary's best_lap:
// ecu's best leaves out pit laps and laps that don't count, where v1's card did
// not (contracts' SessionSummary.best_lap), and the card is v1's.
func BestLap(laps []*eventsv1.LapCompleted, skipLap1 bool, gameBest int) (lap, ms int, ok bool) {
	if gameBest > 0 {
		for _, l := range laps {
			if int(l.GetLapTimeMs()) == gameBest {
				return int(l.GetLap()), gameBest, true
			}
		}
	}
	type timed struct{ lap, ms int }
	var all []timed
	others := false
	for _, l := range laps {
		if l.GetComplete() && l.GetLapTimeMs() > 0 {
			all = append(all, timed{int(l.GetLap()), int(l.GetLapTimeMs())})
			others = others || l.GetLap() != 1
		}
	}
	for _, t := range all {
		if skipLap1 && others && t.lap == 1 {
			continue
		}
		if !ok || t.ms < ms {
			lap, ms, ok = t.lap, t.ms, true
		}
	}
	return lap, ms, ok
}

// TrackLabel is the card's track: the layout's name when ecu is sure of it
// (probable or confirmed), else the circuit's, else "Unknown track".
func TrackLabel(t *eventsv1.TrackVerdict) string {
	switch c := t.GetConfidence(); {
	case (c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE ||
		c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED) && t.GetName() != "":
		return t.GetName()
	case t.GetFamily() != "":
		return t.GetFamily()
	}
	return "Unknown track"
}

// KindLabel is the card's kind of session, from ecu's verdict on it (kind) and
// the laps the race runs: "6-lap race", "timed race", "time trial", "3-lap run".
// A kind ecu could not tell takes the summary's session type (summaryType), as
// v1 took its `type`, else "session".
func KindLabel(kind eventsv1.SessionType, totalLaps int, summaryType eventsv1.SessionType) string {
	switch kind {
	case eventsv1.SessionType_SESSION_TYPE_LAP_RACE:
		return fmt.Sprintf("%d-lap race", totalLaps)
	case eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD:
		return "timed race"
	case eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED:
		return "time trial"
	case eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS:
		return fmt.Sprintf("%d-lap run", totalLaps)
	}
	switch summaryType {
	case eventsv1.SessionType_SESSION_TYPE_RACE:
		return "race"
	case eventsv1.SessionType_SESSION_TYPE_TIME_TRIAL:
		return "time trial"
	case eventsv1.SessionType_SESSION_TYPE_PRACTICE:
		return "practice"
	}
	return "session"
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

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
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
