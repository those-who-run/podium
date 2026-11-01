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
one after another. `podium card --sid SID` reads session `SID`'s
`session.ended` from the `EVENTS` stream at `TWR_NATS_URL` instead, for the
source `--src` (by default `TWR_PODIUM_SRC`, or `live`). `--pitwall URL`, by
default `TWR_PITWALL_URL`, links each card's title to the session on the pit
wall: `<URL>/?session=<sid>`.

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
gateway session (paddock ADR 0010).

## Posting each card once

The service posts the card of each session that ends
([`internal/cards`](internal/cards/cards.go), paddock#67). A durable consumer,
`cards-<src>`, reads `twr.v1.<src>.evt.*.session.ended` from `EVENTS`. For
each session with a card, podium writes `posting` under `<src>.<sid>` in the
`cards` KV bucket (a create-only write), sends the card, writes `posted` with
the Discord message's id, publishes `twr.v1.<src>.evt.<sid>.card.posted`
(`Nats-Msg-Id` `<src>:<sid>:card.posted:1`) for logbook, and acks. A
`session.ended` that finds `posted` posts nothing, so one delivered again, or
published again, posts no second card.

- **Only sessions that end after the consumer is first made.** The consumer
  delivers new messages only, so its first start doesn't post the cards of
  the 30 days of sessions `EVENTS` keeps. NATS can't change a durable
  consumer's deliver policy, so changing this means deleting `cards-<src>`.
  A replay's podium (`TWR_PODIUM_SRC=replay-…`) must therefore be started
  before the replay publishes, or it posts nothing for that replay.
- **One replica.** podium runs as a single replica. The `cards` bucket keeps
  two posters from both sending, but a rate limit's wait or a slow Discord can
  hold a card past the consumer's 5 min ack wait, and a second replica would
  then take the same `session.ended` and wait on the first's `posting` record.
- **Retries, as v1:** three attempts, waiting 2 s then 4 s, or as long as a
  rate limit asks, up to a minute, so a card's attempts end within the ack
  wait. Every send of a session's card carries the same nonce (25 characters
  of sha256 of `<src>:<sid>`) with `enforce_nonce`, so Discord answers a send
  it already took with that message, not a second one. A refused token, a
  missing channel or a missing permission (401, 403, 404) is tried once and
  logged with the hint to check the channel id and the bot's permissions.
  A card whose every attempt Discord refused with a 4xx is counted `failed`,
  loses its `posting` record, and never stops the next. A card with any
  attempt that failed on the way (a timeout, a reset) or with a 5xx, which
  Discord may have taken, keeps its record and is delivered again a minute
  later, to be looked for in the channel.
- **A poster stopped mid-post:** a `posting` record under a minute old is a
  poster still at it, and the `session.ended` waits. An older one is checked
  against the channel's latest 50 messages, only the bot's own and their rich
  embeds, by the card's pit wall link (or its title and start): found, it is
  recorded as `posted` at the message's own time and not sent again; not
  found, it is sent. When the channel can't be read now (Discord down, a rate
  limit), the `session.ended` is delivered again later, after the wait a rate
  limit asks for, up to a minute. When the bot may not read it (401, 403,
  404), podium errs on the side of a missed card rather than a double one,
  logs a warning, and counts it `failed`: a missed card can be posted again by
  hand, while a double card is what this exists to prevent. Reading the
  channel needs the bot's **Read Message History** permission, beside View
  Channel, Send Messages and Embed Links.
- **Starting:** a NATS server that isn't up yet is waited for, not a restart:
  the client keeps trying, and the cards start once it connects. Until that
  first connection `GET /healthz` answers 503, `waiting for NATS`, so the pod
  isn't Ready. After it, the client reconnects by itself and `/healthz` stays
  200. `podium card` doesn't wait: with no server it fails at once.
- **Stopping:** once the server has stopped, podium waits up to 5 s for a card
  in flight to be recorded before it closes its NATS connection.
- **No full lap:** no card and no record, counted `skipped`.
- **The metric:** `GET /metrics` serves `gt7_results_posts_total{result}`
  (`ok`, `failed`, `skipped`), v1's series under its v1 name.

Its settings:

| Variable | What | Default |
| --- | --- | --- |
| `TWR_PODIUM_SINK` | `discord`, or `console`, which writes each card to the log | `discord` |
| `TWR_DISCORD_RESULTS_CHANNEL_ID` | The results channel. Unset with the `discord` sink turns cards off: no consumer, nothing posted (as v1) | unset |
| `TWR_DISCORD_BOT_TOKEN` | The bot's token, radio's name for the shared Secret; needed with a channel | unset |
| `TWR_PITWALL_URL` | The pit wall's base URL each card's title links to | no link |
| `TWR_PODIUM_SRC` | The source whose sessions it posts | `live` |
| `TWR_NATS_URL` | The NATS server; needed when cards are on | unset |

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.

## Conventions

The ones every Go component shares, from the
[`chassis`](https://github.com/those-who-run/chassis) template:

- **Configuration from the environment**, read once at start-up into a typed
  struct; an unset variable takes its default and a value that can't be read
  stops the process. The service reads `ADDR` (`:8080`) and
  `LOG_LEVEL` (`info`), and the cards' settings above.
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs.
- **A health endpoint**, `GET /healthz`, which answers 200 while the process can
  serve (with cards on, from NATS's first connection), and its metrics,
  `GET /metrics`.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-podium:X.Y.Z` once the maintainer approves it in
the `release` environment.
