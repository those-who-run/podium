package card

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

// ReadEnded reads the session.ended events in r: one or more, each in protobuf's
// JSON form as a session.ended message's body holds it, one after another (one
// a line, or each written across lines). Fields it doesn't know are ignored, as
// every consumer of contracts does (ADR 0002, point 3).
func ReadEnded(r io.Reader) ([]*eventsv1.SessionEnded, error) {
	dec := json.NewDecoder(r)
	var out []*eventsv1.SessionEnded
	for n := 1; ; n++ {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("event %d: %w", n, err)
		}
		e := &eventsv1.SessionEnded{}
		if err := ce.Unmarshal(raw, e); err != nil {
			return nil, fmt.Errorf("event %d: not a session.ended: %w", n, err)
		}
		out = append(out, e)
	}
}
