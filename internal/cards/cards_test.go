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

// fakeDiscord is Discord's Create Message and Get Channel Messages for one
// channel: each card sent is a message the channel lists, newest first.
type fakeDiscord struct {
	mu    sync.Mutex
	sent  []discord.Embed
	fails []int // the status of each next send, before one succeeds
	read  int   // the status Get Channel Messages answers with; 0 is 200
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		if f.read != 0 {
			w.WriteHeader(f.read)
			_, _ = io.WriteString(w, `{"message": "Missing Access"}`)
			return
		}
		type msg struct {
			ID     string          `json:"id"`
			Embeds []discord.Embed `json:"embeds"`
		}
		var list []msg
		for i := len(f.sent) - 1; i >= 0; i-- {
			list = append(list, msg{ID: fmt.Sprint(1000 + i), Embeds: []discord.Embed{f.sent[i]}})
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}
	if len(f.fails) > 0 {
		w.WriteHeader(f.fails[0])
		f.fails = f.fails[1:]
		_, _ = io.WriteString(w, `{"message": "no"}`)
		return
	}
	var m struct {
		Embeds []discord.Embed `json:"embeds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&m)
	f.sent = append(f.sent, m.Embeds[0])
	_, _ = fmt.Fprintf(w, `{"id": "%d"}`, 1000+len(f.sent)-1)
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
	publish(t, js, e, endedID(sid))
	stop := run(t, p)
	eventually(t, "one card posted", func() bool { return p.Counters.Get(OK) == 1 && settled(t, js) })
	stop()
	if f.count() != 1 || f.sent[0].Title != "Suzuka Circuit East Course · 6-lap race" {
		t.Fatalf("sent %v", f.sent)
	}
	checkPostedOnce(t, js, kv, sid, "1000")

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
	}{
		{"twice then posted", []int{500, 502}, 3, []time.Duration{2 * time.Second, 4 * time.Second}, true},
		{"three times", []int{500, 500, 500}, 3, []time.Duration{2 * time.Second, 4 * time.Second}, false},
		{"rate limited", []int{429}, 2, []time.Duration{1500 * time.Millisecond}, true},
		{"forbidden", []int{403}, 1, nil, false},
		{"no channel", []int{404}, 1, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var sends int
			var waits []time.Duration
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends++
				if sends <= len(c.fails) {
					if c.fails[sends-1] == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "1.5")
					}
					w.WriteHeader(c.fails[sends-1])
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
			id, err := p.attempt(t.Context(), slog.New(slog.DiscardHandler), card.Card{Title: "T"})
			if (err == nil) != c.ok || (c.ok && id != "7") || sends != c.sends || fmt.Sprint(waits) != fmt.Sprint(c.waits) {
				t.Errorf("%q, %v after %d sends and waits %v; want ok %v after %d sends and waits %v",
					id, err, sends, waits, c.ok, c.sends, c.waits)
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
	publish(t, js, first, endedID(first.GetSessionId()))
	publish(t, js, last, endedID(last.GetSessionId()))
	run(t, p)
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
			publish(t, js, e, endedID(sid))

			if c.sent {
				p1 := newPoster(t, js, kv, f)
				p1.afterSend = func() error { return errStopped }
				stop1 := run(t, p1)
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

type senderFunc func(context.Context, card.Card) (string, error)

func (f senderFunc) Send(ctx context.Context, c card.Card) (string, error) { return f(ctx, c) }

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
	p.Sender = senderFunc(func(context.Context, card.Card) (string, error) { return "", nil })
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
	for _, e := range ended {
		publish(t, js, e, endedID(e.GetSessionId()))
	}
	run(t, p)
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
	run(t, p)
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
