package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"

	"github.com/those-who-run/podium/internal/card"
	"github.com/those-who-run/podium/internal/cards"
	"github.com/those-who-run/podium/internal/sink"
)

// noCard is what `podium card` prints for a session that gets no card, as v1's
// `post --dry-run` did.
const noCard = "no full lap: nothing to post"

// cardCommand is `podium card [--pitwall URL] FILE` or `podium card [--pitwall
// URL] [--src SRC] --sid SID`, v1's `those-who-run post <id> --dry-run`
// (paddock#66, R4): it prints the card of each session.ended in FILE ("-" for
// standard input), or of session SID's session.ended read from the EVENTS
// stream at TWR_NATS_URL, as the console sink writes it, and posts nothing.
// FILE holds one or more session.ended messages' bodies, as stored from the
// EVENTS stream. --pitwall defaults to TWR_PITWALL_URL, --src to
// TWR_PODIUM_SRC or live. It returns the exit code: 0 when a card was printed,
// 1 when no session got one or the session.ended can't be read, 2 for a usage
// error.
func cardCommand(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("podium card", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pitwall := flags.String("pitwall", getenv("TWR_PITWALL_URL"),
		"the pit wall's base URL each card's title links to ($TWR_PITWALL_URL); empty is no link")
	src := getenv("TWR_PODIUM_SRC")
	if src == "" {
		src = "live"
	}
	flags.StringVar(&src, "src", src, "the source whose session --sid names ($TWR_PODIUM_SRC, live when unset)")
	sid := flags.String("sid", "", "read this session's session.ended from the EVENTS stream at $TWR_NATS_URL, not from FILE")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if (*sid == "") != (flags.NArg() == 1) || flags.NArg() > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: podium card [--pitwall URL] FILE | podium card [--pitwall URL] [--src SRC] --sid SID")
		return 2
	}
	var ended []*eventsv1.SessionEnded
	path := flags.Arg(0)
	if *sid != "" {
		url := getenv("TWR_NATS_URL")
		if url == "" {
			_, _ = fmt.Fprintln(stderr, "--sid reads the EVENTS stream: set TWR_NATS_URL")
			return 2
		}
		e, err := readBySID(ctx, url, src, *sid)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		ended, path = []*eventsv1.SessionEnded{e}, *sid
	} else {
		in := stdin
		if path != "-" {
			f, err := os.Open(path) //nolint:gosec // the file the user named
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 1
			}
			defer func() { _ = f.Close() }()
			in = f
		}
		var err error
		if ended, err = card.ReadEnded(in); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, err)
			return 1
		}
	}
	console := sink.NewConsole(stdout)
	printed := 0
	for _, e := range ended {
		c, ok := card.Build(e, *pitwall)
		if !ok {
			_, _ = fmt.Fprintf(stdout, "%s: %s\n", e.GetSessionId(), noCard)
			continue
		}
		if err := console.Post(ctx, e.GetSessionId(), c); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		printed++
	}
	if printed == 0 {
		if len(ended) == 0 {
			_, _ = fmt.Fprintf(stderr, "%s: no session.ended in it\n", path)
		}
		return 1
	}
	return 0
}

// readBySID is readEvents: a var, so the tests can read without a NATS server
// (internal/cards' tests read one with ReadEnded).
var readBySID = readEvents

// readEvents reads a session's session.ended from the EVENTS stream at url.
func readEvents(ctx context.Context, url, src, sid string) (*eventsv1.SessionEnded, error) {
	nc, err := nats.Connect(url, nats.Name("podium card"))
	if err != nil {
		return nil, fmt.Errorf("nats: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return cards.ReadEnded(ctx, js, src, sid)
}
