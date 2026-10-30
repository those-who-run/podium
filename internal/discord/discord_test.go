package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/podium/internal/card"
)

var testCard = card.Card{
	Title: "Sportsland SUGO · 5-lap race", Description: "🚩 Finished P3 of 20 in 8:14.302",
	Fields: []card.Field{{Name: "Best lap", Value: "1:36.595 (lap 9)"}, {Name: "Car", Value: "@everyone's car"}},
	Colour: card.AdvanRed, Footer: "Those Who Run 走り屋",
	Timestamp: time.Date(2026, 10, 18, 19, 0, 0, 0, time.UTC), URL: "http://pw/?session=s",
}

// fake is Discord's Create Message: it records each request and answers with
// status and body.
func fake(t *testing.T, status int, body string) (*httptest.Server, *[]*http.Request, *[]map[string]any) {
	t.Helper()
	var reqs []*http.Request
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		reqs, sent = append(reqs, r), append(sent, m)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &sent
}

func TestPostSendsTheEmbed(t *testing.T) {
	srv, reqs, sent := fake(t, http.StatusOK, `{"id": "1300000000000000001"}`)
	p := &Poster{BaseURL: srv.URL + "/", Token: "tok", ChannelID: "42"}
	id, err := p.Send(t.Context(), testCard)
	if err != nil || id != "1300000000000000001" {
		t.Fatalf("%q, %v", id, err)
	}
	r := (*reqs)[0]
	if r.Method != http.MethodPost || r.URL.Path != "/channels/42/messages" ||
		r.Header.Get("Authorization") != "Bot tok" || r.Header.Get("Content-Type") != "application/json" ||
		!strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
		t.Errorf("request %s %s %v", r.Method, r.URL.Path, r.Header)
	}
	got, _ := json.Marshal((*sent)[0])
	want := `{"allowed_mentions":{"parse":[]},"embeds":[{"color":13770523,` +
		`"description":"🚩 Finished P3 of 20 in 8:14.302",` +
		`"fields":[{"inline":true,"name":"Best lap","value":"1:36.595 (lap 9)"},` +
		`{"inline":true,"name":"Car","value":"@everyone's car"}],` +
		`"footer":{"text":"Those Who Run 走り屋"},"timestamp":"2026-10-18T19:00:00Z",` +
		`"title":"Sportsland SUGO · 5-lap race","url":"http://pw/?session=s"}]}`
	if string(got) != want {
		t.Errorf("sent\n%s\nwant\n%s", got, want)
	}
	if err := p.Post(t.Context(), "s", testCard); err != nil {
		t.Errorf("Post: %v", err)
	}
}

func TestAnEmbedLeavesOutWhatACardHasNot(t *testing.T) {
	data, _ := json.Marshal(EmbedOf(card.Card{Title: "T", Description: "D", Colour: card.CarbonGrey, Footer: "F"}))
	if want := `{"title":"T","description":"D","color":3355443,"footer":{"text":"F"}}`; string(data) != want {
		t.Errorf("%s, want %s", data, want)
	}
}

func TestAFailureSaysWhetherARetryCanHelp(t *testing.T) {
	for status, permanent := range map[int]bool{
		http.StatusUnauthorized: true, http.StatusForbidden: true, http.StatusNotFound: true,
		http.StatusTooManyRequests: false, http.StatusInternalServerError: false, http.StatusBadGateway: false,
	} {
		srv, _, _ := fake(t, status, `{"message": "Missing Access", "code": 50001}`+"\n")
		_, err := (&Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}).Send(t.Context(), testCard)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != status || se.Permanent() != permanent {
			t.Errorf("%d: %v, want a StatusError, permanent %v", status, err, permanent)
			continue
		}
		if !strings.Contains(err.Error(), "Missing Access") || strings.Contains(err.Error(), "tok") {
			t.Errorf("%d: %q should hold Discord's message and not the token", status, err)
		}
	}
}

func TestOtherFailures(t *testing.T) {
	if _, err := (&Poster{ChannelID: "42"}).Send(t.Context(), testCard); err == nil {
		t.Error("no token: no error")
	}
	if _, err := (&Poster{Token: "tok"}).Send(t.Context(), testCard); err == nil {
		t.Error("no channel: no error")
	}
	srv, _, _ := fake(t, http.StatusOK, `not json`)
	if _, err := (&Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}).Send(t.Context(), testCard); err == nil {
		t.Error("an unreadable answer: no error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}).Send(ctx, testCard); err == nil {
		t.Error("cancelled: no error")
	}
	if _, err := (&Poster{BaseURL: "http://\x7f", Token: "tok", ChannelID: "42"}).Send(t.Context(), testCard); err == nil {
		t.Error("a bad URL: no error")
	}
}
