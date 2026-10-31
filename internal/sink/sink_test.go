package sink

import (
	"errors"
	"strings"
	"testing"

	"github.com/those-who-run/podium/internal/card"
)

func TestTheConsolePrintsEachCardAsV1sDryRun(t *testing.T) {
	var b strings.Builder
	s := NewConsole(&b)
	a := card.Card{Title: "A", Description: "2 laps", Fields: []card.Field{{Name: "Car", Value: "NISMO 400R '95"}}}
	c := card.Card{Title: "B", URL: "http://pw/?session=s", Description: "🚩 Finished"}
	for _, x := range []card.Card{a, c} {
		if err := s.Post(t.Context(), "s", x); err != nil {
			t.Fatal(err)
		}
	}
	if want := "A\n2 laps\n  Car: NISMO 400R '95\n\nB\nhttp://pw/?session=s\n🚩 Finished\n"; b.String() != want {
		t.Errorf("%q, want %q", b.String(), want)
	}
}

type broken struct{}

func (broken) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestTheConsoleSaysWhenItCannotWrite(t *testing.T) {
	var s Sink = NewConsole(broken{})
	if err := s.Post(t.Context(), "s", card.Card{Title: "A"}); err == nil {
		t.Error("no error")
	}
}

// paddock#67: Send is Post, for the poster, with no message id.
func TestSendWritesTheCardWithNoID(t *testing.T) {
	var out strings.Builder
	id, err := NewConsole(&out).Send(t.Context(), card.Card{Title: "T · session", Footer: "F"})
	if err != nil || id != "" || !strings.Contains(out.String(), "T · session") {
		t.Errorf("Send = %q, %v; wrote %q", id, err, out.String())
	}
}
