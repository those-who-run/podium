package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/those-who-run/podium/internal/card"
	"github.com/those-who-run/podium/internal/sink"
)

// noCard is what `podium card` prints for a session that gets no card, as v1's
// `post --dry-run` did.
const noCard = "no full lap: nothing to post"

// cardCommand is `podium card [--pitwall URL] FILE`, v1's `those-who-run post <id>
// --dry-run` (paddock#66, R4): it prints the card of each session.ended in FILE
// ("-" for standard input), as the console sink writes it, and posts nothing.
// FILE holds one or more session.ended messages' bodies, as stored from the
// EVENTS stream. --pitwall defaults to TWR_PITWALL_URL. It returns the exit
// code: 0 when a card was printed, 1 when no session got one or FILE can't be
// read, 2 for a usage error.
func cardCommand(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("podium card", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pitwall := flags.String("pitwall", getenv("TWR_PITWALL_URL"),
		"the pit wall's base URL each card's title links to ($TWR_PITWALL_URL); empty is no link")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: podium card [--pitwall URL] FILE")
		return 2
	}
	path := flags.Arg(0)
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
	ended, err := card.ReadEnded(in)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 1
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
