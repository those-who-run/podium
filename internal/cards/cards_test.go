package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"

	"github.com/those-who-run/podium/internal/card"
	"github.com/those-who-run/podium/internal/discord"
	"github.com/those-who-run/podium/internal/natstest"
)

// src is the replay source the tests post for (#67, AC1).
const src = "replay-test"

// The session.ended events ecu published for the recordings, as #66 stored
// them (internal/card's testdata).
func fixture(t *testing.T, name string) []*eventsv1.SessionEnded {
	t.Helper()
	f, err := os.Open("../card/testdata/ended/" + name + ".jsonl") //nolint:gosec // a fixed test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ended, err := card.ReadEnded(f)
	if err != nil {
		t.Fatal(err)
	}
	return ended
}

// publish publishes a session.ended on src's subject with the id msgID.
func publish(t *testing.T, js jetstream.JetStream, e *eventsv1.SessionEnded, msgID string) {
	t.Helper()
	h, data, err := ce.Encode(e, ce.Event{ID: msgID, Source: "/twr/ecu/gt7", Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	subject := "twr.v1." + src + ".evt." + e.GetSessionId() + ".session.ended"
	if _, err := js.PublishMsg(t.Context(), &nats.Msg{Subject: subject, Header: nats.Header(h), Data: data}); err != nil {
		t.Fatal(err)
	}
}

func endedID(sid string) string { return src + ":" + sid + ":session.ended:1" }

// fakeDiscord is Discord's Create Message, Get Channel Messages and Get
// Current User for one channel and the bot "bot": each card sent is a message
// the channel lists, newest first. A send with a nonce it has had before
// answers with that message, as Discord's enforce_nonce does.
type fakeDiscord struct {
	mu      sync.Mutex
	sent    []discord.Embed
	nonces  []string // the nonce of each send that reached it
	byNonce map[string]int
	fails   []int // the status of each next send, before one succeeds
	lose    int   // how many of the next answers to a send are lost (the connection cut)
	read    int   // the status Get Channel Messages answers with; 0 is 200
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet && r.URL.Path == "/users/@me" {
		_, _ = io.WriteString(w, `{"id": "bot"}`)
		return
	}
	if r.Method == http.MethodGet {
		if f.read != 0 {
			w.WriteHeader(f.read)
			_, _ = io.WriteString(w, `{"message": "no"}`)
			return
		}
		type author struct {
			ID string `json:"id"`
		}
		type msg struct {
			ID     string          `json:"id"`
			Author author          `json:"author"`
			Embeds []discord.Embed `json:"embeds"`
		}
		var list []msg
		for i := len(f.sent) - 1; i >= 0; i-- {
			e := f.sent[i]
			e.Type = "rich"
			list = append(list, msg{ID: fmt.Sprint(1000 + i), Author: author{"bot"}, Embeds: []discord.Embed{e}})
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}
	var m struct {
		Embeds       []discord.Embed `json:"embeds"`
		Nonce        string          `json:"nonce"`
		EnforceNonce bool            `json:"enforce_nonce"`
	}
	_ = json.NewDecoder(r.Body).Decode(&m)
	f.nonces = append(f.nonces, m.Nonce)
	if len(f.fails) > 0 {
		w.WriteHeader(f.fails[0])
		f.fails = f.fails[1:]
		_, _ = io.WriteString(w, `{"message": "no"}`)
		return
	}
	n, seen := f.byNonce[m.Nonce]
	if !seen || !m.EnforceNonce {
		f.sent = append(f.sent, m.Embeds[0])
		n = len(f.sent) - 1
		if f.byNonce == nil {
			f.byNonce = map[string]int{}
		}
		f.byNonce[m.Nonce] = n
	}
	if f.lose > 0 {
		f.lose--
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	_, _ = fmt.Fprintf(w, `{"id": "%d"}`, 1000+n)
}

func (f *fakeDiscord) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// newPoster is a poster for src to a fake Discord, with no waits between
// attempts and a 1 s ack wait.
func newPoster(t *testing.T, js jetstream.JetStream, kv jetstream.KeyValue, f *fakeDiscord) *Poster {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Poster{
		JS: js, KV: kv, Src: src, Pitwall: "http://pitwall.test",
		Sender:   &discord.Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"},
		Counters: &Counters{}, Logger: slog.New(slog.DiscardHandler),
		sleep:   func(context.Context, time.Duration) error { return nil },
		ackWait: time.Second,
	}
}

// run runs p until the returned stop is called, which waits for Run to return.
func run(t *testing.T, p *Poster) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	// The consumer delivers only what is published once it exists.
	eventually(t, "the consumer made", func() bool {
		_, err := p.JS.Consumer(t.Context(), Stream, Consumer(src))
		return err == nil
	})
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not after 15 s: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settled is whether the consumer has acked everything the stream holds for
// it.
func settled(t *testing.T, js jetstream.JetStream) bool {
	t.Helper()
	c, err := js.Consumer(t.Context(), Stream, Consumer(src))
	if err != nil {
		return false
	}
	info, err := c.Info(t.Context())
	return err == nil && info.NumAckPending == 0 && info.NumPending == 0
}

func record(t *testing.T, kv jetstream.KeyValue, sid string) *statev1.CardRecord {
	t.Helper()
	entry, err := kv.Get(t.Context(), Key(src, sid))
	if err != nil {
		t.Fatalf("%s: %v", Key(src, sid), err)
	}
	rec := &statev1.CardRecord{}
	if err := ce.Unmarshal(entry.Value(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// cardPosted is every card.posted the stream holds for sid.
func cardPosted(t *testing.T, js jetstream.JetStream, sid string) []*jetstream.RawStreamMsg {
	t.Helper()
	st, err := js.Stream(t.Context(), Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []*jetstream.RawStreamMsg
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		m, err := st.GetMsg(t.Context(), seq)
		if err == nil && m.Subject == "twr.v1."+src+".evt."+sid+".card.posted" {
			out = append(out, m)
		}
	}
	return out
}

// checkPostedOnce checks sid's card was sent once, is recorded as posted with
// its message id, and card.posted is in the stream once.
func checkPostedOnce(t *testing.T, js jetstream.JetStream, kv jetstream.KeyValue, sid, id string) {
	t.Helper()
	rec := record(t, kv, sid)
	if rec.GetState() != statev1.CardState_CARD_STATE_POSTED || rec.GetMessageId() != id ||
		rec.GetSource() != src || rec.GetSessionId() != sid || rec.GetAt() == nil {
		t.Errorf("the record is %v, want posted as %s", rec, id)
	}
	posted := cardPosted(t, js, sid)
	if len(posted) != 1 {
		t.Fatalf("%d card.posted, want 1", len(posted))
	}
	m := &eventsv1.CardPosted{}
	e, err := ce.Decode(ce.Header(posted[0].Header), posted[0].Data, m)
	if err != nil {
		t.Fatal(err)
	}
	wantID := src + ":" + sid + ":card.posted:1"
	if e.ID != wantID || posted[0].Header.Get(nats.MsgIdHdr) != wantID || e.Source != "/twr/podium" ||
		m.GetSessionId() != sid || m.GetMessageId() != id || !m.GetPostedAt().AsTime().Equal(rec.GetAt().AsTime()) {
		t.Errorf("card.posted is %v %v, want %s for message %s", e, m, wantID, id)
	}
}

// AC1: the Suzuka no-stop race's session.ended on a replay source posts one
// card, records the sid, and publishes card.posted once. AC2: the same
// session.ended published again with a new Nats-Msg-Id, and delivered again
// by not acking it, posts nothing more, and both are acked.
func TestEachCardIsPostedOnce(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{}
	p := newPoster(t, js, kv, f)
	e := fixture(t, "real-nuc-suzuka-no-stop-2026-10-15")[0]
	sid := e.GetSessionId()
	stop := run(t, p)
	publish(t, js, e, endedID(sid))
	eventually(t, "one card posted", func() bool { return p.Counters.Get(OK) == 1 && settled(t, js) })
	stop()
	if f.count() != 1 || f.sent[0].Title != "Suzuka Circuit East Course · 6-lap race" {
		t.Fatalf("sent %v", f.sent)
	}
	checkPostedOnce(t, js, kv, sid, "1000")
	if want := Nonce(src, sid); len(f.nonces) != 1 || f.nonces[0] != want || len(want) != 25 {
		t.Errorf("sent with nonces %v, want %s", f.nonces, want)
	}

	// AC2: a new Nats-Msg-Id, so the stream's dedupe doesn't catch it, then
	// a delivery the test takes and never acks, so the stream delivers it
	// again after the ack wait.
	publish(t, js, e, endedID(sid)+":again")
	c, err := js.Consumer(t.Context(), Stream, Consumer(src))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := c.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for range batch.Messages() {
		// Not acked.
	}
	time.Sleep(1200 * time.Millisecond)
	p2 := newPoster(t, js, kv, f)
	run(t, p2)
	eventually(t, "the second session.ended acked", func() bool { return settled(t, js) })
	if f.count() != 1 || p2.Counters.Get(OK) != 0 || p2.Counters.Get(Failed) != 0 {
		t.Errorf("sent %d cards, counted %d ok and %d failed: want 1 card, nothing more counted",
			f.count(), p2.Counters.Get(OK), p2.Counters.Get(Failed))
	}
	checkPostedOnce(t, js, kv, sid, "1000")
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.NumRedelivered != 0 || info.Delivered.Consumer < 3 {
		t.Errorf("the consumer delivered %d (%d redelivered), want 3 deliveries of 2 messages, all acked",
			info.Delivered.Consumer, info.NumRedelivered)
	}
}

// AC3: Discord failing twice then taking the card posts it once, after waits
// of 2 s and 4 s; a rate limit's wait is honoured; Forbidden is tried once.
func TestRetriesAsV1(t *testing.T) {
	for _, c := range []struct {
		name  string
		fails []int
		sends int
		waits []time.Duration
		ok    bool
		maybe bool // whether a failed attempt may have reached the channel
	}{
		{"twice then posted", []int{500, 502}, 3, []time.Duration{2 * time.Second, 4 * time.Second}, true, true},
		{"three times", []int{500, 500, 500}, 3, []time.Duration{2 * time.Second, 4 * time.Second}, false, true},
		{"rate limited", []int{429}, 2, []time.Duration{1500 * time.Millisecond}, true, false},
		{"rate limited for long", []int{-429}, 2, []time.Duration{maxWait}, true, false},
		{"rate limited three times", []int{429, 429, 429}, 3, []time.Duration{1500 * time.Millisecond, 1500 * time.Millisecond}, false, false},
		{"forbidden", []int{403}, 1, nil, false, false},
		{"a 502, then forbidden", []int{502, 403}, 2, []time.Duration{2 * time.Second}, false, true},
		{"no channel", []int{404}, 1, nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var sends int
			var waits []time.Duration
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends++
				if sends <= len(c.fails) {
					status := c.fails[sends-1]
					switch status {
					case http.StatusTooManyRequests:
						w.Header().Set("Retry-After", "1.5")
					case -http.StatusTooManyRequests:
						status = http.StatusTooManyRequests
						w.Header().Set("Retry-After", "600")
					}
					w.WriteHeader(status)
					return
				}
				_, _ = io.WriteString(w, `{"id": "7"}`)
			}))
			defer srv.Close()
			p := &Poster{
				Sender: &discord.Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"},
				sleep: func(_ context.Context, d time.Duration) error {
					waits = append(waits, d)
					return nil
				},
			}
			id, maybe, err := p.attempt(t.Context(), slog.New(slog.DiscardHandler), "n", card.Card{Title: "T"})
			if (err == nil) != c.ok || (c.ok && id != "7") || sends != c.sends || fmt.Sprint(waits) != fmt.Sprint(c.waits) ||
				maybe != c.maybe {
				t.Errorf("%q, maybe taken %v, %v after %d sends and waits %v; want ok %v, maybe taken %v after %d sends and waits %v",
					id, maybe, err, sends, waits, c.ok, c.maybe, c.sends, c.waits)
			}
		})
	}
}

// AC3 on the bus: a Forbidden card is counted failed, loses its posting
// record, and doesn't stop the next card.
func TestAFailedCardIsCountedAndTheNextPosted(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{fails: []int{http.StatusForbidden}}
	p := newPoster(t, js, kv, f)
	ended := fixture(t, "real-nuc-sugo-restarts-2026-10-18")
	first, last := ended[0], ended[2]
	run(t, p)
	publish(t, js, first, endedID(first.GetSessionId()))
	publish(t, js, last, endedID(last.GetSessionId()))
	eventually(t, "both handled", func() bool { return settled(t, js) && p.Counters.Get(OK) == 1 })
	if p.Counters.Get(Failed) != 1 || f.count() != 1 {
		t.Errorf("%d failed, %d sent; want 1 and 1", p.Counters.Get(Failed), f.count())
	}
	if _, err := kv.Get(t.Context(), Key(src, first.GetSessionId())); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the failed card's record: %v, want none", err)
	}
	checkPostedOnce(t, js, kv, last.GetSessionId(), "1000")
}

// AC4: a poster stopped between the send and the record leaves "posting".
// The next poster, once it is a minute old, finds the card in the channel and
// records it, sending nothing; when the channel can't be read it errs on the
// side of a missed card and sends nothing either; when the channel hasn't
// the card, the poster stopped before the send, and it is posted.
func TestAPosterStoppedAfterTheSend(t *testing.T) {
	for _, c := range []struct {
		name     string
		read     int
		sent     bool // whether the stopped poster had sent the card
		sends    int
		state    statev1.CardState
		ok, fail uint64
	}{
		{"found in the channel", 0, true, 1, statev1.CardState_CARD_STATE_POSTED, 1, 0},
		{"the channel unreadable", http.StatusForbidden, true, 1, statev1.CardState_CARD_STATE_POSTING, 0, 1},
		{"stopped before the send", 0, false, 1, statev1.CardState_CARD_STATE_POSTED, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			js, kv := natstest.Start(t)
			f := &fakeDiscord{read: c.read}
			e := fixture(t, "real-nuc-suzuka-no-stop-2026-10-15")[0]
			sid := e.GetSessionId()

			if c.sent {
				p1 := newPoster(t, js, kv, f)
				p1.afterSend = func() error { return errStopped }
				stop1 := run(t, p1)
				publish(t, js, e, endedID(sid))
				eventually(t, "the card sent", func() bool { return f.count() == 1 })
				stop1()
			} else {
				// Stopped after the posting record and before the send.
				p0 := &Poster{Src: src}
				if _, err := kv.Create(t.Context(), Key(src, sid), p0.record(sid, statev1.CardState_CARD_STATE_POSTING, "")); err != nil {
					t.Fatal(err)
				}
			}
			if rec := record(t, kv, sid); rec.GetState() != statev1.CardState_CARD_STATE_POSTING {
				t.Fatalf("the stopped poster left %v, want posting", rec)
			}

			p2 := newPoster(t, js, kv, f)
			p2.now = func() time.Time { return time.Now().Add(2 * stale) }
			run(t, p2)
			if !c.sent {
				publish(t, js, e, endedID(sid))
			}
			eventually(t, "the session.ended acked", func() bool { return settled(t, js) })
			if f.count() != c.sends || p2.Counters.Get(OK) != c.ok || p2.Counters.Get(Failed) != c.fail {
				t.Errorf("%d sent, %d ok, %d failed; want %d, %d, %d", f.count(), p2.Counters.Get(OK),
					p2.Counters.Get(Failed), c.sends, c.ok, c.fail)
			}
			if rec := record(t, kv, sid); rec.GetState() != c.state {
				t.Errorf("the record is %v, want %v", rec, c.state)
			}
			if c.state == statev1.CardState_CARD_STATE_POSTED {
				checkPostedOnce(t, js, kv, sid, "1000")
			} else if n := len(cardPosted(t, js, sid)); n != 0 {
				t.Errorf("%d card.posted for a card not recorded", n)
			}
		})
	}
}

type senderFunc func(context.Context, string, card.Card) (string, error)

func (f senderFunc) Send(ctx context.Context, nonce string, c card.Card) (string, error) {
	return f(ctx, nonce, c)
}

// R3: a "posting" record under a minute old is another poster still at it,
// so the session.ended waits and nothing is sent; a sink that can't look in
// the channel leaves a stale one as a missed card; a record that isn't one
// posts nothing.
func TestARecordFoundBeforeTheSend(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{}
	p := newPoster(t, js, kv, f)
	log := slog.New(slog.DiscardHandler)
	c := card.Card{Title: "T"}
	if _, err := kv.Create(t.Context(), Key(src, "young"), p.record("young", statev1.CardState_CARD_STATE_POSTING, "")); err != nil {
		t.Fatal(err)
	}
	if err := p.post(t.Context(), log, "young", c); !errors.Is(err, errLater) || f.count() != 0 {
		t.Errorf("a young posting record: %v after %d sends, want later and none", err, f.count())
	}
	p.Sender = senderFunc(func(context.Context, string, card.Card) (string, error) { return "", nil })
	p.now = func() time.Time { return time.Now().Add(2 * stale) }
	if err := p.post(t.Context(), log, "young", c); !errors.Is(err, errGivenUp) || p.Counters.Get(Failed) != 1 {
		t.Errorf("a stale record and a sink that can't look: %v, %d failed", err, p.Counters.Get(Failed))
	}
	for _, bad := range []string{"not json", `{"state": "CARD_STATE_UNSPECIFIED"}`} {
		if _, err := kv.Put(t.Context(), Key(src, "bad"), []byte(bad)); err != nil {
			t.Fatal(err)
		}
		if err := p.post(t.Context(), log, "bad", c); !errors.Is(err, errGivenUp) {
			t.Errorf("a record %q: %v, want given up", bad, err)
		}
	}
}

// AC5: the SUGO restarts recording: the lap-1 restart's session is skipped,
// with no record, and the other two are posted once each.
func TestASessionWithNoFullLapIsSkipped(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{}
	p := newPoster(t, js, kv, f)
	ended := fixture(t, "real-nuc-sugo-restarts-2026-10-18")
	run(t, p)
	for _, e := range ended {
		publish(t, js, e, endedID(e.GetSessionId()))
	}
	eventually(t, "three handled", func() bool { return settled(t, js) && p.Counters.Get(OK) == 2 })
	if p.Counters.Get(Skipped) != 1 || p.Counters.Get(Failed) != 0 || f.count() != 2 {
		t.Errorf("%d skipped, %d failed, %d sent; want 1, 0 and 2", p.Counters.Get(Skipped), p.Counters.Get(Failed), f.count())
	}
	if _, err := kv.Get(t.Context(), Key(src, ended[1].GetSessionId())); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the skipped session's record: %v, want none", err)
	}
	checkPostedOnce(t, js, kv, ended[0].GetSessionId(), "1000")
	checkPostedOnce(t, js, kv, ended[2].GetSessionId(), "1001")
	var metrics strings.Builder
	if _, err := p.Counters.WriteTo(&metrics); err != nil || !strings.Contains(metrics.String(),
		"gt7_results_posts_total{result=\"ok\"} 2\ngt7_results_posts_total{result=\"failed\"} 0\n"+
			"gt7_results_posts_total{result=\"skipped\"} 1\n") {
		t.Errorf("metrics %v:\n%s", err, metrics.String())
	}
}

// A message on the subject that isn't a session.ended, or whose session id
// can't be a key, is dropped, and the next is posted.
func TestWhatIsNotASessionEndedIsDropped(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{}
	p := newPoster(t, js, kv, f)
	run(t, p)
	if _, err := js.Publish(t.Context(), "twr.v1."+src+".evt.x.session.ended", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	bad := fixture(t, "real-nuc-suzuka-no-stop-2026-10-15")[0]
	bad.SessionId = "a.b"
	h, data, err := ce.Encode(bad, ce.Event{ID: "bad", Source: "/twr/ecu/gt7", Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.PublishMsg(t.Context(), &nats.Msg{
		Subject: "twr.v1." + src + ".evt.y.session.ended", Header: nats.Header(h), Data: data,
	}); err != nil {
		t.Fatal(err)
	}
	e := fixture(t, "real-nuc-suzuka-no-stop-2026-10-15")[0]
	publish(t, js, e, endedID(e.GetSessionId()))
	eventually(t, "all handled", func() bool { return settled(t, js) && p.Counters.Get(OK) == 1 })
	if f.count() != 1 {
		t.Errorf("%d sent, want 1", f.count())
	}
}

func TestRunRefusesASourceThatIsNotAToken(t *testing.T) {
	p := &Poster{Src: "live.x"}
	if err := p.Run(t.Context()); err == nil {
		t.Error("a source with a dot is no error")
	}
}

// paddock#66, R4: a session's session.ended read back by its id.
func TestReadEnded(t *testing.T) {
	js, _ := natstest.Start(t)
	e := fixture(t, "real-nuc-suzuka-no-stop-2026-10-15")[0]
	publish(t, js, e, endedID(e.GetSessionId()))
	got, err := ReadEnded(t.Context(), js, src, e.GetSessionId())
	if err != nil || got.GetSessionId() != e.GetSessionId() || got.GetSummary().GetLapsCompleted() != e.GetSummary().GetLapsCompleted() {
		t.Fatalf("ReadEnded = %v, %v", got, err)
	}
	if _, err := ReadEnded(t.Context(), js, src, "01NOSUCHSESSION"); !errors.Is(err, ErrNoSession) {
		t.Errorf("an unknown session: %v, want ErrNoSession", err)
	}
	if _, err := ReadEnded(t.Context(), js, "live", "a.b"); err == nil {
		t.Error("a session id with a dot is no error")
	}
}

// With no EVENTS stream there is nothing to consume, and Run says so.
func TestRunNeedsTheStream(t *testing.T) {
	s := natstest.Bare(t)
	p := &Poster{JS: s.JS, Src: src, Logger: slog.New(slog.DiscardHandler)}
	if err := p.Run(t.Context()); err == nil || !strings.Contains(err.Error(), Consumer(src)) {
		t.Errorf("Run = %v, want the consumer's error", err)
	}
}

// The consumer posts only the sessions that end once it exists: a first start
// doesn't post the 30 days of cards EVENTS keeps.
func TestOnlyNewSessionsArePosted(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{}
	p := newPoster(t, js, kv, f)
	ended := fixture(t, "real-nuc-sugo-restarts-2026-10-18")
	old, fresh := ended[0], ended[2]
	publish(t, js, old, endedID(old.GetSessionId()))
	run(t, p)
	publish(t, js, fresh, endedID(fresh.GetSessionId()))
	eventually(t, "the new card posted", func() bool { return settled(t, js) && p.Counters.Get(OK) == 1 })
	if f.count() != 1 {
		t.Errorf("%d sent, want 1", f.count())
	}
	if _, err := kv.Get(t.Context(), Key(src, old.GetSessionId())); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the session before the consumer: %v, want no record", err)
	}
	checkPostedOnce(t, js, kv, fresh.GetSessionId(), "1000")
}

// The review of podium#17: a channel that can't be read for now (a 500, a
// rate limit) is asked again later, honouring the rate limit's wait, and
// nothing is counted failed (one the bot may not read gives the card up:
// TestAPosterStoppedAfterTheSend).
func TestAChannelThatCannotBeReadNow(t *testing.T) {
	js, kv := natstest.Start(t)
	f := &fakeDiscord{read: http.StatusInternalServerError}
	p := newPoster(t, js, kv, f)
	p.now = func() time.Time { return time.Now().Add(2 * stale) }
	log := slog.New(slog.DiscardHandler)
	c := card.Card{Title: "T"}
	if _, err := kv.Create(t.Context(), Key(src, "s"), (&Poster{Src: src}).record("s", statev1.CardState_CARD_STATE_POSTING, "")); err != nil {
		t.Fatal(err)
	}
	var later *laterError
	if err := p.post(t.Context(), log, "s", c); !errors.As(err, &later) || later.delay != stale ||
		p.Counters.Get(Failed) != 0 || f.count() != 0 {
		t.Errorf("a 500: %v, %d failed, %d sent; want later in a minute, nothing failed or sent", err, p.Counters.Get(Failed), f.count())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/@me" {
			_, _ = io.WriteString(w, `{"id": "bot"}`)
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p.Sender = &discord.Poster{BaseURL: srv.URL, Token: "tok", ChannelID: "42"}
	if err := p.post(t.Context(), log, "s", c); !errors.As(err, &later) || later.delay != 7*time.Second || p.Counters.Get(Failed) != 0 {
		t.Errorf("a 429: %v, want later in 7 s", err)
	}
	if rec := record(t, kv, "s"); rec.GetState() != statev1.CardState_CARD_STATE_POSTING || p.Counters.Get(Failed) != 0 {
		t.Errorf("the record is %v and %d failed: want posting, none failed", rec, p.Counters.Get(Failed))
	}
}

// The review of podium#17: a send Discord took whose answer was lost is sent
// again with the same nonce, which Discord answers with the message it has:
// one card. When every answer is lost the posting record stays, the
// session.ended is delivered again later, and the card is found in the
// channel and recorded at the message's own time.
func TestALostAnswerPostsOneCard(t *testing.T) {
	js, kv := natstest.Start(t)
	log := slog.New(slog.DiscardHandler)
	c := card.Card{Title: "T"}

	f := &fakeDiscord{lose: 1}
	p := newPoster(t, js, kv, f)
	if err := p.post(t.Context(), log, "one", c); err != nil || f.count() != 1 || len(f.nonces) != 2 ||
		f.nonces[0] != f.nonces[1] || f.nonces[0] != Nonce(src, "one") {
		t.Errorf("one answer lost: %v, %d cards, nonces %v; want one card, sent twice with one nonce", err, f.count(), f.nonces)
	}

	f = &fakeDiscord{lose: attempts}
	p = newPoster(t, js, kv, f)
	var later *laterError
	if err := p.post(t.Context(), log, "all", c); !errors.As(err, &later) || later.delay != stale ||
		f.count() != 1 || p.Counters.Get(Failed) != 0 {
		t.Errorf("every answer lost: %v, %d cards, %d failed; want later, one card, none failed", err, f.count(), p.Counters.Get(Failed))
	}
	if rec := record(t, kv, "all"); rec.GetState() != statev1.CardState_CARD_STATE_POSTING {
		t.Fatalf("the record is %v, want posting kept", rec)
	}
	p.now = func() time.Time { return time.Now().Add(2 * stale) }
	if err := p.post(t.Context(), log, "all", c); err != nil || f.count() != 1 {
		t.Errorf("delivered again: %v, %d cards; want found, one card", err, f.count())
	}
	rec := record(t, kv, "all")
	created, _ := discord.Created("1000")
	if rec.GetState() != statev1.CardState_CARD_STATE_POSTED || rec.GetMessageId() != "1000" || !rec.GetAt().AsTime().Equal(created) {
		t.Errorf("the record is %v, want posted as 1000 at %v", rec, created)
	}
}

// The re-review of podium#19: a send that times out after Discord stored the
// card, then two 502s, keeps the posting record and is delivered again later,
// to go through Find; nothing is counted failed. A card every attempt of which
// Discord refused with a 4xx loses its record and is counted failed.
func TestATimeoutThenGatewayErrorsKeepsTheRecord(t *testing.T) {
	js, kv := natstest.Start(t)
	log := slog.New(slog.DiscardHandler)
	c := card.Card{Title: "T"}
	p := newPoster(t, js, kv, &fakeDiscord{})
	sends := 0
	p.Sender = senderFunc(func(context.Context, string, card.Card) (string, error) {
		sends++
		if sends == 1 {
			return "", errors.New("discord: context deadline exceeded")
		}
		return "", &discord.StatusError{Status: http.StatusBadGateway}
	})
	var later *laterError
	if err := p.post(t.Context(), log, "timeout", c); !errors.As(err, &later) || sends != attempts || p.Counters.Get(Failed) != 0 {
		t.Errorf("timeout, then 502, then 502: %v after %d sends, %d failed; want later, %d sends, none failed",
			err, sends, p.Counters.Get(Failed), attempts)
	}
	if rec := record(t, kv, "timeout"); rec.GetState() != statev1.CardState_CARD_STATE_POSTING {
		t.Errorf("the record is %v, want posting kept", rec)
	}

	p.Sender = senderFunc(func(context.Context, string, card.Card) (string, error) {
		return "", &discord.StatusError{Status: http.StatusTooManyRequests, RetryAfter: time.Second}
	})
	if err := p.post(t.Context(), log, "refused", c); !errors.Is(err, errGivenUp) || p.Counters.Get(Failed) != 1 {
		t.Errorf("three 429s: %v, %d failed; want given up, 1 failed", err, p.Counters.Get(Failed))
	}
	if _, err := kv.Get(t.Context(), Key(src, "refused")); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the refused card's record: %v, want none", err)
	}
}

// A rate limit on reading the channel is waited out for at most maxWait,
// since every card queues behind it.
func TestRetryLaterIsCapped(t *testing.T) {
	var later *laterError
	if !errors.As(retryLater(&discord.StatusError{Status: http.StatusTooManyRequests, RetryAfter: 24 * time.Hour}), &later) ||
		later.delay != maxWait {
		t.Errorf("a 24 h Retry-After waits %v, want %v", later.delay, maxWait)
	}
}
