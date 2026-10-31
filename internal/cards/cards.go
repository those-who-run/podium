// Package cards posts each session's results card once (paddock#67): a durable
// consumer of session.ended on the EVENTS stream, the cards KV bucket keyed
// <src>.<sid> so a session.ended delivered again posts nothing, v1's retries,
// and card.posted for logbook's posted_at.
//
// For each session.ended it builds the card (internal/card) and, unless the
// session gets none, writes "posting" to the bucket with a create-only write,
// sends the card, writes "posted" with a revision check, publishes card.posted
// and acks. A session.ended that finds "posted" posts nothing. One that finds
// "posting" written more than a minute before (a poster stopped between the
// send and the record) asks the channel whether the card is there before it
// sends again; when the channel can't be read it errs on the side of a missed
// card rather than a double one, since the driver can post a missed card again
// by hand but a double card is the failure this exists to prevent.
package cards

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	statev1 "github.com/those-who-run/contracts/gen/go/twr/state/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/podium/internal/card"
	"github.com/those-who-run/podium/internal/discord"
)

// Stream is the stream session.ended is kept in, and card.posted goes to
// (paddock ADR 0001, point 1).
const Stream = "EVENTS"

// Bucket is the KV bucket of posted cards (contracts' subjects.yaml).
const Bucket = "cards"

// ceSource is the ce-source of what podium publishes.
const ceSource = "/twr/podium"

// v1's ResultsPoster: three attempts, the wait doubling from 2 s (R4).
const (
	attempts = 3
	backoff  = 2 * time.Second
)

// stale is how old a "posting" record must be before a poster that finds it
// checks the channel and takes the card over (R3): a poster still sending
// writes "posted" well within it.
const stale = time.Minute

// ackWait is how long the stream waits for an ack before it delivers a
// session.ended again: longer than three attempts that each time out (30 s)
// and the waits between them.
const ackWait = 5 * time.Minute

// Sender sends a card and returns its message id, empty when it has none:
// discord.Poster, or sink.Console for replays and tests.
type Sender interface {
	Send(ctx context.Context, c card.Card) (string, error)
}

// Finder looks for a card already in the channel (discord.Poster). A Sender
// that isn't one can't be asked, and a stale "posting" record is then left
// as a missed card.
type Finder interface {
	Find(ctx context.Context, c card.Card) (id string, found bool, err error)
}

// Poster posts the cards of one source's sessions.
type Poster struct {
	// JS is the JetStream context, for the consumer and card.posted.
	JS jetstream.JetStream
	// KV is the cards bucket.
	KV jetstream.KeyValue
	// Src is the source whose sessions it posts: live, or a replay's.
	Src string
	// Sender is where the cards go.
	Sender Sender
	// Pitwall is the pit wall's base URL each card's title links to; empty is
	// no link.
	Pitwall string
	// Counters counts the results (R5); nil counts nothing.
	Counters *Counters
	// Logger is where it logs.
	Logger *slog.Logger

	// The tests' seams: the clock, the wait between attempts, and the stream's
	// ack wait. Zero values are time.Now, a wait that ends with ctx, and ackWait.
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	ackWait   time.Duration
	afterSend func() error
}

// Consumer is the durable consumer's name for a source: cards-<src> (#67's
// interfaces).
func Consumer(src string) string { return "cards-" + src }

// token is a subject or key token podium accepts for a source or a session
// id: letters, digits, - and _.
var token = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Run creates the durable consumer, or takes up the one there, and posts the
// card of each session.ended it delivers until ctx is done. A card that fails
// never stops the next (R4). It returns an error only when it cannot consume.
func (p *Poster) Run(ctx context.Context) error {
	if !token.MatchString(p.Src) {
		return fmt.Errorf("cards: source %q is not letters, digits, - and _", p.Src)
	}
	wait := p.ackWait
	if wait == 0 {
		wait = ackWait
	}
	cons, err := p.JS.CreateOrUpdateConsumer(ctx, Stream, jetstream.ConsumerConfig{
		Durable:       Consumer(p.Src),
		Description:   "podium: the results card of each session (paddock#67)",
		FilterSubject: "twr.v1." + p.Src + ".evt.*.session.ended",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       wait,
		// One card at a time, in order: a card is a few seconds' work.
		MaxAckPending: 1,
	})
	if err != nil {
		return fmt.Errorf("cards: consumer %s: %w", Consumer(p.Src), err)
	}
	msgs, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("cards: consumer %s: %w", Consumer(p.Src), err)
	}
	stopped := context.AfterFunc(ctx, msgs.Stop)
	defer stopped()
	p.Logger.InfoContext(ctx, "posting cards", "src", p.Src, "consumer", Consumer(p.Src))
	for {
		msg, err := msgs.Next()
		if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
			return nil
		}
		if err != nil {
			p.Logger.WarnContext(ctx, "cards: waiting for session.ended", "err", err)
			_ = p.pause(ctx, time.Second)
			continue
		}
		p.Handle(ctx, msg)
	}
}

// Handle posts the card of one session.ended, and acks, naks or terms it.
func (p *Poster) Handle(ctx context.Context, msg jetstream.Msg) {
	ended := &eventsv1.SessionEnded{}
	if _, err := ce.Decode(ce.Header(msg.Headers()), msg.Data(), ended); err != nil {
		p.Logger.ErrorContext(ctx, "cards: not a session.ended, dropped", "subject", msg.Subject(), "err", err)
		_ = msg.Term()
		return
	}
	sid := ended.GetSessionId()
	log := p.Logger.With("src", p.Src, "session", sid)
	if !token.MatchString(sid) {
		log.ErrorContext(ctx, "cards: the session id can't be a key, dropped")
		_ = msg.Term()
		return
	}
	c, ok := card.Build(ended, p.Pitwall)
	if !ok {
		log.InfoContext(ctx, "no full lap: no card")
		p.Counters.add(Skipped)
		_ = msg.Ack()
		return
	}
	switch err := p.post(ctx, log, sid, c); {
	case err == nil:
		_ = msg.Ack()
	case errors.Is(err, errLater):
		// Another poster may still be sending, or the record couldn't be
		// read or written: try again once a stale record would be stale.
		_ = msg.NakWithDelay(stale)
	case errors.Is(err, errStopped):
		// Stopped mid-post: nothing acked, so the stream delivers it again.
	default:
		_ = msg.Ack()
	}
}

var (
	// errLater is a session.ended to deliver again later.
	errLater = errors.New("later")
	// errStopped is a post cut short by the poster stopping.
	errStopped = errors.New("stopped")
	// errGivenUp is a card that is not posted: acked, counted failed.
	errGivenUp = errors.New("given up")
)

// post posts c once, by the record in the bucket.
func (p *Poster) post(ctx context.Context, log *slog.Logger, sid string, c card.Card) error {
	key := Key(p.Src, sid)
	entry, err := p.KV.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		rev, err := p.KV.Create(ctx, key, p.record(sid, statev1.CardState_CARD_STATE_POSTING, ""))
		if errors.Is(err, jetstream.ErrKeyExists) {
			// Another poster got there first: look again later.
			return errLater
		}
		if err != nil {
			log.ErrorContext(ctx, "cards: the posting record is not written", "err", err)
			return errLater
		}
		return p.send(ctx, log, key, sid, rev, c)
	case err != nil:
		log.ErrorContext(ctx, "cards: the record is unreadable", "err", err)
		return errLater
	}
	rec := &statev1.CardRecord{}
	if err := ce.Unmarshal(entry.Value(), rec); err != nil {
		log.ErrorContext(ctx, "cards: the record is not a CardRecord; nothing posted", "err", err)
		return errGivenUp
	}
	switch rec.GetState() {
	case statev1.CardState_CARD_STATE_POSTED:
		// Posted already: card.posted again, which the stream drops as a
		// duplicate within its window, in case the poster stopped before it.
		log.InfoContext(ctx, "card posted already", "message_id", rec.GetMessageId())
		p.published(ctx, log, sid, rec.GetAt().AsTime(), rec.GetMessageId())
		return nil
	case statev1.CardState_CARD_STATE_POSTING:
	default:
		log.ErrorContext(ctx, "cards: the record has no state; nothing posted", "state", rec.GetState().String())
		return errGivenUp
	}
	if age := p.clock().Sub(rec.GetAt().AsTime()); age < stale {
		log.InfoContext(ctx, "card being posted: looking again later", "age", age.String())
		return errLater
	}
	// R3: a poster stopped between the send and the record, or before the
	// send. Ask the channel.
	finder, ok := p.Sender.(Finder)
	if !ok {
		log.WarnContext(ctx, "card maybe posted by a poster that stopped, and this sink can't tell: not posting it again")
		p.Counters.add(Failed)
		return errGivenUp
	}
	id, found, err := finder.Find(ctx, c)
	if err != nil {
		log.WarnContext(ctx, "card maybe posted by a poster that stopped, and the channel can't be read: "+
			"not posting it again (a missed card rather than a double one). Check the bot's Read Message History permission",
			"err", err)
		p.Counters.add(Failed)
		return errGivenUp
	}
	if found {
		log.InfoContext(ctx, "card found in the channel: recording it", "message_id", id)
		return p.posted(ctx, log, key, sid, entry.Revision(), id)
	}
	rev, err := p.KV.Update(ctx, key, p.record(sid, statev1.CardState_CARD_STATE_POSTING, ""), entry.Revision())
	if err != nil {
		log.ErrorContext(ctx, "cards: the stale posting record is not taken over", "err", err)
		return errLater
	}
	log.InfoContext(ctx, "card not in the channel: posting it")
	return p.send(ctx, log, key, sid, rev, c)
}

// send sends c with v1's retries, then records it as posted. A card that
// fails every attempt, or fails for good, loses its posting record, so a
// later post by hand can try again, and is counted failed.
func (p *Poster) send(ctx context.Context, log *slog.Logger, key, sid string, rev uint64, c card.Card) error {
	id, err := p.attempt(ctx, log, c)
	if err != nil {
		if ctx.Err() != nil {
			return errStopped
		}
		if derr := p.KV.Delete(context.WithoutCancel(ctx), key, jetstream.LastRevision(rev)); derr != nil {
			log.WarnContext(ctx, "cards: the posting record is left", "err", derr)
		}
		p.Counters.add(Failed)
		return errGivenUp
	}
	if p.afterSend != nil {
		if err := p.afterSend(); err != nil {
			return err
		}
	}
	return p.posted(ctx, log, key, sid, rev, id)
}

// attempt sends c up to three times, waiting 2 s then 4 s between attempts, or
// as long as a rate limit asks; a refusal no retry can fix (401, 403, 404) is
// tried once.
func (p *Poster) attempt(ctx context.Context, log *slog.Logger, c card.Card) (string, error) {
	wait := backoff
	var err error
	for n := 1; n <= attempts; n++ {
		var id string
		if id, err = p.Sender.Send(ctx, c); err == nil {
			return id, nil
		}
		var status *discord.StatusError
		if errors.As(err, &status) && status.Permanent() {
			log.ErrorContext(ctx, "card not posted: check the channel id and the bot's permissions "+
				"(View Channel, Send Messages, Embed Links)", "err", err)
			return "", err
		}
		if n == attempts || ctx.Err() != nil {
			break
		}
		d := wait
		if errors.As(err, &status) && status.RetryAfter > 0 {
			d = status.RetryAfter
		}
		log.WarnContext(ctx, "card not posted: trying again", "attempt", n, "wait", d.String(), "err", err)
		if err := p.pause(ctx, d); err != nil {
			return "", err
		}
		wait *= 2
	}
	log.ErrorContext(ctx, "card not posted", "attempts", attempts, "err", err)
	return "", err
}

// posted records the card as posted, publishes card.posted, and counts it.
func (p *Poster) posted(ctx context.Context, log *slog.Logger, key, sid string, rev uint64, id string) error {
	at := p.clock()
	// The card is in the channel: the record is written even if the poster
	// is stopping.
	ctx = context.WithoutCancel(ctx)
	if _, err := p.KV.Update(ctx, key, p.recordAt(sid, statev1.CardState_CARD_STATE_POSTED, id, at), rev); err != nil {
		// Left as posting: a later delivery finds it stale and the card in
		// the channel.
		log.ErrorContext(ctx, "cards: card posted, its record not written", "message_id", id, "err", err)
		return errLater
	}
	log.InfoContext(ctx, "card posted", "message_id", id)
	p.Counters.add(OK)
	p.published(ctx, log, sid, at, id)
	return nil
}

// published publishes card.posted (R6), with the id <src>:<sid>:card.posted:1,
// so the stream keeps one of a card.posted sent twice within its duplicate
// window.
func (p *Poster) published(ctx context.Context, log *slog.Logger, sid string, at time.Time, id string) {
	m := &eventsv1.CardPosted{SessionId: sid, PostedAt: timestamppb.New(at), MessageId: id}
	h, data, err := ce.Encode(m, ce.Event{ID: p.Src + ":" + sid + ":card.posted:1", Source: ceSource, Time: at})
	if err != nil {
		log.ErrorContext(ctx, "cards: card.posted not encoded", "err", err)
		return
	}
	msg := &nats.Msg{Subject: "twr.v1." + p.Src + ".evt." + sid + ".card.posted", Header: nats.Header(h), Data: data}
	if _, err := p.JS.PublishMsg(ctx, msg); err != nil {
		log.ErrorContext(ctx, "cards: card.posted not published", "err", err)
	}
}

func (p *Poster) record(sid string, state statev1.CardState, id string) []byte {
	return p.recordAt(sid, state, id, p.clock())
}

func (p *Poster) recordAt(sid string, state statev1.CardState, id string, at time.Time) []byte {
	data, err := ce.Marshal(&statev1.CardRecord{
		Source: p.Src, SessionId: sid, State: state, At: timestamppb.New(at), MessageId: id,
	})
	if err != nil {
		// A CardRecord of strings, an enum and a time always encodes.
		panic(err)
	}
	return data
}

func (p *Poster) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Poster) pause(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Key is a session's key in the bucket: <src>.<sid>.
func Key(src, sid string) string { return strings.Join([]string{src, sid}, ".") }
