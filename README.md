# podium

The results cards for Those Who Run v2: when a session ends, it posts the
session's results card to Discord over its REST API. It is the home for
everything posted after a session.

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here
names its paddock issue: `Closes those-who-run/paddock#N.` when it meets every
acceptance criterion, `Part of those-who-run/paddock#N` otherwise
([`AGENTS.md`](AGENTS.md)).

## The card

A session's card is built from its `session.ended` alone
([`internal/card`](internal/card/card.go)): v1 1.0.0's results card, field for
field, with its times and fuel figures written by contracts' `format` package.
A session with no full lap gets none.

`podium card FILE` prints the card of each `session.ended` in `FILE` (`-` for
standard input), as v1's `those-who-run post <id> --dry-run` did, and posts
nothing. `FILE` holds the messages' bodies as the `EVENTS` stream stores them,
one after another. `--pitwall URL`, by default `TWR_PITWALL_URL`, links each
card's title to the session on the pit wall: `<URL>/?session=<sid>`.

```console
$ podium card --pitwall http://pitwall.home.arpa ended.jsonl
Fuji International Speedway · time trial
http://pitwall.home.arpa/?session=01KDVDNA000ZB038PDRJACDC76
2 laps
  Best lap: 2:00.314 (lap 2)
  Car: NISMO 400R '95
```

A card goes to a sink ([`internal/sink`](internal/sink/sink.go)): the console,
which needs no Discord, or the results channel through Discord's REST API
([`internal/discord`](internal/discord/discord.go)), with the bot token and no
gateway session (paddock ADR 0010). Posting each session's card from the bus,
exactly once, is paddock#67.

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.

## Conventions

The ones every Go component shares, from the
[`chassis`](https://github.com/those-who-run/chassis) template:

- **Configuration from the environment**, read once at start-up into a typed
  struct; an unset variable takes its default and a value that can't be read
  stops the process. The service reads `ADDR` (`:8080`) and
  `LOG_LEVEL` (`info`).
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs.
- **A health endpoint**, `GET /healthz`, which answers 200 while the process can
  serve.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-podium:X.Y.Z` once the maintainer approves it in
the `release` environment.
