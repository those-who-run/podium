package discord

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/podium/internal/card"
)

// paddock#67, R3: Find reads the channel's latest messages and finds a card by
// its link, or by its title and start when it has none.
func TestFindLooksForTheCardInTheChannel(t *testing.T) {
	unlinked := testCard
	unlinked.URL = ""
	messages := `[
		{"id": "1", "embeds": [{"title": "Other · session", "url": "http://pw/?session=other"}]},
		{"id": "2", "embeds": []},
		{"id": "3", "embeds": [{"title": "Sportsland SUGO · 5-lap race", "url": "http://pw/?session=s"}]},
		{"id": "4", "embeds": [{"title": "Sportsland SUGO · 5-lap race", "timestamp": "2026-10-18T19:00:00+00:00"}]}
	]`
	srv, reqs, _ := fake(t, http.StatusOK, messages)
	p := &Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}
	for _, c := range []struct {
		name string
		card card.Card
		id   string
		ok   bool
	}{
		{"by its link", testCard, "3", true},
		{"by its title and start", unlinked, "4", true},
		{"another start", func() card.Card { c := unlinked; c.Timestamp = c.Timestamp.Add(time.Second); return c }(), "", false},
		{"no start", func() card.Card { c := unlinked; c.Timestamp = time.Time{}; return c }(), "", false},
		{"another link", func() card.Card { c := testCard; c.URL = "http://pw/?session=t"; return c }(), "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, ok, err := p.Find(t.Context(), c.card)
			if err != nil || id != c.id || ok != c.ok {
				t.Errorf("Find = %q, %v, %v; want %q, %v", id, ok, err, c.id, c.ok)
			}
		})
	}
	r := (*reqs)[0]
	if r.Method != http.MethodGet || r.URL.Path != "/channels/42/messages" || r.URL.Query().Get("limit") != "50" ||
		r.Header.Get("Authorization") != "Bot tok" {
		t.Errorf("request %s %s %v", r.Method, r.URL, r.Header)
	}
}

func TestFindFailures(t *testing.T) {
	srv, _, _ := fake(t, http.StatusForbidden, `{"message": "Missing Access"}`)
	_, _, err := (&Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard)
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusForbidden || strings.Contains(err.Error(), "tok") {
		t.Errorf("a 403 is %v", err)
	}
	limited, _, _ := fake(t, http.StatusTooManyRequests, `{"retry_after": 1.5}`)
	_, _, err = (&Poster{BaseURL: limited.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard)
	if !errors.As(err, &status) || status.RetryAfter != 1500*time.Millisecond {
		t.Errorf("a 429 is %v", err)
	}
	garbled, _, _ := fake(t, http.StatusOK, `{"not": "a list"}`)
	if _, _, err := (&Poster{BaseURL: garbled.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("an answer that isn't a list of messages is no error")
	}
	if _, _, err := (&Poster{}).Find(t.Context(), testCard); err == nil {
		t.Error("no token and channel is no error")
	}
	if _, _, err := (&Poster{BaseURL: "http://127.0.0.1:1", Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("a Discord that can't be reached is no error")
	}
	if _, _, err := (&Poster{BaseURL: "://bad", Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("a bad base URL is no error")
	}
}
