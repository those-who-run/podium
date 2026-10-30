package card

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/contracts/format"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The cases of v1's tests/test_results.py for build_card and its helpers, and
// the ones it left to the real sessions, each on a summary made for it.

func i32(v int32) *int32 { return &v }

func laps(times ...int32) []*eventsv1.LapCompleted {
	out := make([]*eventsv1.LapCompleted, len(times))
	for i, ms := range times {
		out[i] = &eventsv1.LapCompleted{Lap: int32(i + 1), LapTimeMs: ms, Complete: ms > 0}
	}
	return out
}

// solo is v1's SUMMARY: three 100 s laps of a solo timed session, finished.
func solo() *eventsv1.SessionEnded {
	return &eventsv1.SessionEnded{
		SessionId: "20261016-190000",
		Reason:    eventsv1.EndReason_END_REASON_FINISHED,
		Summary: &eventsv1.SessionSummary{
			LapsCompleted: 3, Finished: true, TotalTimeMs: i32(300000),
			Laps: laps(100000, 100000, 100000),
			Kind: &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED},
		},
	}
}

// race is a 5-lap race with a field at SUGO, P3 of 20 after 2 laps.
func race() *eventsv1.SessionEnded {
	return &eventsv1.SessionEnded{
		SessionId: "01KDVDNA000ZB038PDRJACDC76",
		Reason:    eventsv1.EndReason_END_REASON_QUIT,
		Summary: &eventsv1.SessionSummary{
			LapsCompleted: 2, TotalLaps: i32(5), FinalPosition: i32(3), TotalCars: i32(20),
			Laps: laps(101000, 99000, 0),
			Kind: &eventsv1.SessionKind{
				SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: i32(5),
				Field: true, CarsOnTrack: true, FuelUse: eventsv1.FuelUse_FUEL_USE_ON,
			},
			Track: &eventsv1.TrackVerdict{
				Name: "Sportsland SUGO", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED,
			},
			FuelBurned: 40, FuelCapacity: 100,
		},
	}
}

func build(t *testing.T, e *eventsv1.SessionEnded) Card {
	t.Helper()
	c, ok := Build(e, "")
	if !ok {
		t.Fatal("no card")
	}
	return c
}

func TestNoCardWithoutAFullLap(t *testing.T) {
	if _, ok := Build(&eventsv1.SessionEnded{SessionId: "x", Summary: &eventsv1.SessionSummary{}}, ""); ok {
		t.Error("a card for no full lap")
	}
	if _, ok := Build(&eventsv1.SessionEnded{SessionId: "x"}, ""); ok {
		t.Error("a card for no summary")
	}
}

func TestASoloSessionsCard(t *testing.T) {
	c := build(t, solo())
	want := Card{
		Title: "Unknown track · time trial", Description: "🚩 Finished in 5:00.000",
		Fields: []Field{{"Best lap", "1:40.000 (lap 1)"}}, Colour: AdvanRed, Footer: "Those Who Run 走り屋",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got %+v\nwant %+v", c, want)
	}
}

func TestTheResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(e *eventsv1.SessionEnded)
		want string
	}{
		{"retired from a lap race", func(*eventsv1.SessionEnded) {}, "Retired on lap 3 of 5, running P3"},
		{"retired on the last lap", func(e *eventsv1.SessionEnded) { e.Summary.LapsCompleted = 5 }, "Retired on lap 5 of 5, running P3"},
		{"finished", func(e *eventsv1.SessionEnded) {
			e.Summary.Finished, e.Summary.TotalTimeMs = true, i32(494302)
		}, "🚩 Finished P3 of 20 in 8:14.302"},
		{"finished with no time", func(e *eventsv1.SessionEnded) { e.Summary.Finished = true }, "🚩 Finished P3 of 20"},
		{"finished with no position", func(e *eventsv1.SessionEnded) {
			e.Summary.Finished, e.Summary.Kind.CarsOnTrack = true, false
		}, "🚩 Finished"},
		{"finished with no field size", func(e *eventsv1.SessionEnded) {
			e.Summary.Finished, e.Summary.TotalCars = true, nil
		}, "🚩 Finished P3"},
		{"ended paused", func(e *eventsv1.SessionEnded) { e.Summary.EndedPaused = true }, "Paused on lap 3 of 5, P3"},
		{"ended paused in a timed race", func(e *eventsv1.SessionEnded) {
			e.Summary.EndedPaused, e.Summary.TotalLaps = true, nil
		}, "Paused on lap 3, P3"},
		{"ended paused on the last lap", func(e *eventsv1.SessionEnded) {
			e.Summary.EndedPaused, e.Summary.LapsCompleted = true, 5
		}, "Paused on lap 5 of 5, P3"},
		{"engineer stopped", func(e *eventsv1.SessionEnded) {
			e.Reason = eventsv1.EndReason_END_REASON_ENGINEER_STOPPED
		}, "Engineer stopped on lap 3 of 5, P3"},
		{"engineer stopped with no laps", func(e *eventsv1.SessionEnded) {
			e.Reason, e.Summary.Laps, e.Summary.TotalLaps = eventsv1.EndReason_END_REASON_ENGINEER_STOPPED, nil, nil
		}, "Engineer stopped on lap 3, P3"},
		{"engineer stopped in a pause", func(e *eventsv1.SessionEnded) {
			e.Reason, e.Summary.EndedPaused = eventsv1.EndReason_END_REASON_ENGINEER_STOPPED, true
		}, "Paused on lap 3 of 5, P3"},
		{"a timed race", func(e *eventsv1.SessionEnded) {
			e.Summary.Kind.SessionType, e.Summary.TotalLaps = eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, nil
		}, "2 laps, P3 of 20"},
		{"one lap", func(e *eventsv1.SessionEnded) {
			e.Summary.Kind.SessionType, e.Summary.LapsCompleted = eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, 1
		}, "1 lap, P3 of 20"},
		{"no field", func(e *eventsv1.SessionEnded) {
			e.Summary.Kind.SessionType, e.Summary.Kind.Field = eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS, false
		}, "2 laps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := race()
			tc.edit(e)
			if got := build(t, e).Description; got != tc.want {
				t.Errorf("%q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheColourIsRedOnlyWhenFinished(t *testing.T) {
	if c := build(t, race()); c.Colour != CarbonGrey {
		t.Errorf("colour %06X, want carbon grey", c.Colour)
	}
	e := race()
	e.Summary.Finished = true
	if c := build(t, e); c.Colour != AdvanRed || !strings.HasPrefix(c.Description, GlyphFlag+" ") {
		t.Errorf("colour %06X, %q: want Advan red and the flag", c.Colour, c.Description)
	}
}

func TestTheFields(t *testing.T) {
	e := race()
	e.Summary.CarName = "Mazda RX-VISION GT3 EVO '26"
	e.Summary.JoinedOnLap = i32(2)
	e.Summary.Gaps = []*eventsv1.SessionGap{{Lap: 7}, {Lap: 0}, {Lap: 9}}
	e.Summary.FuelBurnedByStint = []float64{97.4, 64.5}
	want := []Field{
		{"Best lap", "1:39.000 (lap 2)"}, {"Car", "Mazda RX-VISION GT3 EVO '26"}, {"Joined", "on lap 2"},
		{"Engineer restarted", "on laps 7, 9"}, {"Fuel used", "97%, then 64%"},
	}
	if got := build(t, e).Fields; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestFuelUsedOnlyWithFuelUseOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(s *eventsv1.SessionSummary)
		want string
	}{
		{"one stint", func(*eventsv1.SessionSummary) {}, "40%"},
		{"halves to even, as v1's round", func(s *eventsv1.SessionSummary) { s.FuelBurned = 42.5 }, "42%"},
		{"fuel use off", func(s *eventsv1.SessionSummary) { s.Kind.FuelUse = eventsv1.FuelUse_FUEL_USE_OFF }, ""},
		{"fuel use unknown", func(s *eventsv1.SessionSummary) { s.Kind.FuelUse = eventsv1.FuelUse_FUEL_USE_UNKNOWN }, ""},
		{"none burnt", func(s *eventsv1.SessionSummary) { s.FuelBurned = 0 }, ""},
		{"no tank", func(s *eventsv1.SessionSummary) { s.FuelCapacity = 0 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := race()
			tc.edit(e.Summary)
			got := ""
			for _, f := range build(t, e).Fields {
				if f.Name == "Fuel used" {
					got = f.Value
				}
			}
			if got != tc.want {
				t.Errorf("%q, want %q", got, tc.want)
			}
		})
	}
}

func TestBestLap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		laps     []*eventsv1.LapCompleted
		skip     bool
		gameBest int
		lap, ms  int
		ok       bool
	}{
		{"the quickest", laps(101000, 99000, 100000), false, 0, 2, 99000, true},
		{"lap 1 counts outside a lap race", laps(90000, 99000), false, 0, 1, 90000, true},
		{"lap 1 of a lap race is skipped", laps(90000, 99000), true, 0, 2, 99000, true},
		{"unless it is the only one timed", laps(90000, 0), true, 0, 1, 90000, true},
		{"GT7's best wins", laps(90000, 99000, 98000), false, 99000, 2, 99000, true},
		{"GT7's best on no lap seen", laps(90000, 99000), false, 95000, 1, 90000, true},
		{"none timed", laps(0, 0), false, 0, 0, 0, false},
		{"no laps", nil, true, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lap, ms, ok := BestLap(tc.laps, tc.skip, tc.gameBest)
			if lap != tc.lap || ms != tc.ms || ok != tc.ok {
				t.Errorf("(%d, %d, %v), want (%d, %d, %v)", lap, ms, ok, tc.lap, tc.ms, tc.ok)
			}
		})
	}
}

func TestTrackNameNeedsConfidenceOrAFamily(t *testing.T) {
	unsure := &eventsv1.TrackVerdict{
		Name: "Fuji International Speedway (Short)", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS,
	}
	family := proto.Clone(unsure).(*eventsv1.TrackVerdict)
	family.Family = "Fuji International Speedway"
	probable := &eventsv1.TrackVerdict{Name: "Fuji", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE}
	nameless := &eventsv1.TrackVerdict{Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}
	for track, want := range map[*eventsv1.TrackVerdict]string{
		unsure: "Unknown track", family: "Fuji International Speedway", probable: "Fuji",
		nameless: "Unknown track", nil: "Unknown track",
	} {
		if got := TrackLabel(track); got != want {
			t.Errorf("%v: %q, want %q", track, got, want)
		}
	}
}

func TestKindLabel(t *testing.T) {
	for _, tc := range []struct {
		kind eventsv1.SessionType
		laps int
		want string
	}{
		{eventsv1.SessionType_SESSION_TYPE_LAP_RACE, 5, "5-lap race"},
		{eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, 0, "timed race"},
		{eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, 0, "time trial"},
		{eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS, 3, "3-lap run"},
		{eventsv1.SessionType_SESSION_TYPE_UNKNOWN, 0, "session"},
		{eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED, 0, "session"},
	} {
		if got := KindLabel(tc.kind, tc.laps); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.kind, got, tc.want)
		}
	}
	e := race()
	e.Summary.Kind = nil
	e.Summary.SessionType = eventsv1.SessionType_SESSION_TYPE_LAP_RACE // ignored: kind is the verdict
	if c := build(t, e); c.Title != "Sportsland SUGO · session" || c.Description != "2 laps" {
		t.Errorf("no kind: %q, %q", c.Title, c.Description)
	}
}

func TestSessionLink(t *testing.T) {
	for _, tc := range [][3]string{
		{"", "20261017-162118", ""},
		{"http://pw", "", ""},
		{"http://pw", "20261017-162118", "http://pw/?session=20261017-162118"},
		{"http://pw//", "a b&c/d", "http://pw/?session=a%20b%26c%2Fd"},
		{"http://pw", "走~", "http://pw/?session=%E8%B5%B0~"},
	} {
		if got := SessionLink(tc[0], tc[1]); got != tc[2] {
			t.Errorf("SessionLink(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestOnlyTheLinkChangesWithAPitWall(t *testing.T) {
	plain := build(t, race())
	linked, _ := Build(race(), "http://pitwall.home.arpa/")
	if want := "http://pitwall.home.arpa/?session=01KDVDNA000ZB038PDRJACDC76"; linked.URL != want {
		t.Errorf("link %q, want %q", linked.URL, want)
	}
	linked.URL = ""
	if !reflect.DeepEqual(plain, linked) {
		t.Errorf("more than the link changed:\n%+v\n%+v", plain, linked)
	}
	if strings.Contains(plain.Text(), "?session=") {
		t.Error("a link in the text without a pit wall")
	}
}

func TestTheTimestampIsTheStart(t *testing.T) {
	e := race()
	at := time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC)
	e.Summary.StartedAt = timestamppb.New(at)
	if got := build(t, e).Timestamp; !got.Equal(at) {
		t.Errorf("timestamp %v, want %v", got, at)
	}
	e.Summary.StartedAt = timestamppb.New(time.Unix(0, 0))
	if got := build(t, e).Timestamp; !got.IsZero() {
		t.Errorf("timestamp %v at the epoch, want none", got)
	}
}

func TestTheTextIsV1sDryRun(t *testing.T) {
	c := Card{
		Title: "T", URL: "http://pw/?session=s", Description: "D",
		Fields: []Field{{"A", "1"}, {"B", "2"}},
	}
	if want := "T\nhttp://pw/?session=s\nD\n  A: 1\n  B: 2\n"; c.Text() != want {
		t.Errorf("%q, want %q", c.Text(), want)
	}
}

func TestReadEnded(t *testing.T) {
	in := `{"sessionId": "a", "summary": {"lapsCompleted": 1}, "later": true}
{
  "sessionId": "b"
}`
	got, err := ReadEnded(strings.NewReader(in))
	if err != nil || len(got) != 2 || got[0].GetSessionId() != "a" || got[1].GetSessionId() != "b" {
		t.Fatalf("%v, %v", got, err)
	}
	for _, bad := range []string{`{"sessionId": 3}`, `{`, `[1]`} {
		if _, err := ReadEnded(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// AC5: every case of contracts' format-cases.json that names v1's
// results.race_time, which the card writes its times with.
func TestTheRaceTimeCases(t *testing.T) {
	var file struct {
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(format.Cases(), &file); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, raw := range file.Cases {
		// Another function's args and out may be other than a number and a string.
		var c struct {
			Fn     string `json:"fn"`
			Args   []any  `json:"args"`
			Out    any    `json:"out"`
			Python string `json:"python"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c.Python != "results.race_time" {
			continue
		}
		n++
		ms, ok := c.Args[0].(float64)
		if !ok || len(c.Args) != 1 {
			t.Fatalf("%s: args %v, want one number", c.Fn, c.Args)
		}
		if got := format.RaceTime(int(ms)); got != c.Out {
			t.Errorf("%s(%v) = %q, want %v", c.Fn, ms, got, c.Out)
		}
	}
	if n < 5 {
		t.Fatalf("%d race_time cases, want at least 5", n)
	}
}
