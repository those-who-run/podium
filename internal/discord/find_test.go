package discord

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/podium/internal/card"
)

// fakeChannel answers GET /users/@me with the bot and GET
// /channels/42/messages with messages, counting each request by path.
func fakeChannel(t *testing.T, me, messages string, status int) (*httptest.Server, map[string]int) {
	t.Helper()
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked[r.URL.Path]++
		if r.Header.Get("Authorization") != "Bot tok" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			_, _ = io.WriteString(w, me)
		case "/channels/42/messages":
			if r.URL.Query().Get("limit") != "50" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if status != 0 {
				w.Header().Set("Retry-After", "1.5")
				w.WriteHeader(status)
			}
			_, _ = io.WriteString(w, messages)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, asked
}

// paddock#67, R3: Find reads the channel's latest messages and finds the
// bot's own card, in a rich embed, by its link, or by its title and start
// when it has none.
func TestFindLooksForTheCardInTheChannel(t *testing.T) {
	unlinked := testCard
	unlinked.URL = ""
	messages := `[
		{"id": "1", "author": {"id": "bot"}, "embeds": [{"type": "rich", "title": "Other · session", "url": "http://pw/?session=other"}]},
		{"id": "2", "author": {"id": "bot"}, "embeds": []},
		{"id": "5", "author": {"id": "someone"}, "embeds": [{"type": "rich", "url": "http://pw/?session=s"}]},
		{"id": "6", "author": {"id": "bot"}, "embeds": [{"type": "link", "url": "http://pw/?session=s"}]},
		{"id": "3", "author": {"id": "bot"}, "embeds": [{"type": "rich", "title": "Sportsland SUGO · 5-lap race", "url": "http://pw/?session=s"}]},
		{"id": "4", "author": {"id": "bot"}, "embeds": [{"type": "rich", "title": "Sportsland SUGO · 5-lap race", "timestamp": "2026-10-18T19:00:00+00:00"}]}
	]`
	srv, asked := fakeChannel(t, `{"id": "bot"}`, messages, 0)
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
	if asked["/users/@me"] != 1 {
		t.Errorf("asked who the bot is %d times, want once", asked["/users/@me"])
	}
}

func TestFindFailures(t *testing.T) {
	forbidden, _ := fakeChannel(t, `{"id": "bot"}`, `{"message": "Missing Access"}`, http.StatusForbidden)
	_, _, err := (&Poster{BaseURL: forbidden.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard)
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusForbidden || strings.Contains(err.Error(), "tok") {
		t.Errorf("a 403 is %v", err)
	}
	limited, _ := fakeChannel(t, `{"id": "bot"}`, `{}`, http.StatusTooManyRequests)
	_, _, err = (&Poster{BaseURL: limited.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard)
	if !errors.As(err, &status) || status.RetryAfter != 1500*time.Millisecond {
		t.Errorf("a 429 is %v", err)
	}
	garbled, _ := fakeChannel(t, `{"id": "bot"}`, `{"not": "a list"}`, 0)
	if _, _, err := (&Poster{BaseURL: garbled.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("an answer that isn't a list of messages is no error")
	}
	nobody, _ := fakeChannel(t, `{}`, `[]`, 0)
	if _, _, err := (&Poster{BaseURL: nobody.URL, Token: "tok", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("a bot with no id is no error")
	}
	if _, _, err := (&Poster{BaseURL: forbidden.URL, Token: "wrong", ChannelID: "42"}).Find(t.Context(), testCard); err == nil {
		t.Error("a refused token is no error")
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
