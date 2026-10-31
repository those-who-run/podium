package cards

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// ErrNoSession is ReadEnded's error for a session with no session.ended in
// the stream: none was published, or the stream has aged it out (30 days).
var ErrNoSession = errors.New("no session.ended in EVENTS")

// ReadEnded reads a session's session.ended from the EVENTS stream by its
// source and id, for `podium card --sid` (paddock#66, R4).
func ReadEnded(ctx context.Context, js jetstream.JetStream, src, sid string) (*eventsv1.SessionEnded, error) {
	if !token.MatchString(src) || !token.MatchString(sid) {
		return nil, fmt.Errorf("source %q or session %q is not letters, digits, - and _", src, sid)
	}
	st, err := js.Stream(ctx, Stream)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Stream, err)
	}
	subject := "twr.v1." + src + ".evt." + sid + ".session.ended"
	m, err := st.GetLastMsgForSubject(ctx, subject)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return nil, fmt.Errorf("%s: %w", subject, ErrNoSession)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	ended := &eventsv1.SessionEnded{}
	if _, err := ce.Decode(ce.Header(m.Header), m.Data, ended); err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	return ended, nil
}
