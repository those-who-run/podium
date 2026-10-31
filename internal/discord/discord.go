// Package discord posts results cards to a Discord text channel over Discord's
// REST API, "Create Message" with an embed, authenticated with a bot token
// (paddock ADR 0010, point 2). It opens no gateway session and needs no
// privileged intents: the bot needs View Channel, Send Messages and Embed Links
// on the channel, as v1's did.
//
// A Poster sends once. Retries, and posting each card exactly once, are its
// caller's (paddock#67): StatusError's Permanent says which failures a retry
// cannot fix, and its RetryAfter how long a rate limit asks to wait. A message
// Discord took is never reported as a failure, even when its id can't be read,
// so a retry never posts a card twice.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/those-who-run/podium/internal/card"
)

// DefaultBaseURL is Discord's REST API, version 10.
const DefaultBaseURL = "https://discord.com/api/v10"

// userAgent is the User-Agent Discord asks a bot to send: "DiscordBot (URL,
// version)".
const userAgent = "DiscordBot (https://github.com/those-who-run/podium, 1)"

// timeout bounds one request, so a Discord that hangs delays a card, never
// holds anything up for good.
const timeout = 30 * time.Second

// maxBody is the most of a response podium reads: an error's body is kept for
// the log, a message's for its id.
const maxBody = 64 << 10

// Poster posts cards to one channel as one bot.
type Poster struct {
	// BaseURL is the API's base URL: DefaultBaseURL, or a fake's in the tests.
	BaseURL string
	// Token is the bot's token, sent as "Authorization: Bot <token>". It is
	// never logged or put in an error.
	Token string
	// ChannelID is the results channel's id.
	ChannelID string
	// HTTP is the client it sends with; nil is one with a 30 s timeout.
	HTTP *http.Client
	// Logger is where it warns of a message sent whose id it couldn't read; nil
	// is slog.Default().
	Logger *slog.Logger

	// me is the bot's own user id, read once by Find (GET /users/@me).
	mu sync.Mutex
	me string
}

// StatusError is a response from Discord other than a success.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the start of the response's body: Discord's JSON error.
	Body string
	// RetryAfter is how long Discord asks for before the next try, on a 429
	// (rate limited): its Retry-After header, else the body's retry_after, both
	// in seconds. Zero when it gave none.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("discord: %d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

// Permanent says whether trying again cannot help: the token is refused (401),
// the bot may not post in the channel (403), or there is no such channel (404),
// as v1 tried NotFound and Forbidden once. Check the channel id and the bot's
// permissions.
func (e *StatusError) Permanent() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
}

// Post sends c to the channel. sid is for the caller's logs; the card carries
// everything Discord shows.
func (p *Poster) Post(ctx context.Context, _ string, c card.Card) error {
	_, err := p.Send(ctx, "", c)
	return err
}

// MaxNonce is the longest nonce Discord takes: 25 characters.
const MaxNonce = 25

// Send sends c to the channel and returns the message's id. A nonce, when
// given, is sent with enforce_nonce: Discord then answers a send it has
// already taken from this bot in the last few minutes with that message rather
// than a second one, so a retry after a send that timed out but arrived posts
// one card (paddock#67). It is at most MaxNonce characters.
func (p *Poster) Send(ctx context.Context, nonce string, c card.Card) (string, error) {
	if p.Token == "" || p.ChannelID == "" {
		return "", errors.New("discord: posting needs a bot token and a channel id")
	}
	if len(nonce) > MaxNonce {
		return "", fmt.Errorf("discord: the nonce %q is longer than %d characters", nonce, MaxNonce)
	}
	body, err := json.Marshal(message{
		Embeds: []Embed{EmbedOf(c)}, AllowedMentions: allowedMentions{Parse: []string{}},
		Nonce: nonce, EnforceNonce: nonce != "",
	})
	if err != nil {
		return "", fmt.Errorf("discord: %w", err)
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	url := strings.TrimRight(base, "/") + "/channels/" + p.ChannelID + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("discord: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discord: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
		if resp.StatusCode == http.StatusTooManyRequests {
			e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), data)
		}
		return "", e
	}
	// The message was sent: a caller that retried now would post it twice
	// (paddock#67), so an id that can't be read is a warning, not a failure.
	var sent struct {
		ID string `json:"id"`
	}
	if readErr == nil {
		readErr = json.Unmarshal(data, &sent)
	}
	if readErr != nil || sent.ID == "" {
		logger := p.Logger
		if logger == nil {
			logger = slog.Default()
		}
		// Neither the body nor the error, which may quote it, is logged.
		why := "no id in the answer"
		if readErr != nil {
			why = "the answer is unreadable"
		}
		logger.WarnContext(ctx, "discord: card posted, its message id unknown",
			"status", resp.StatusCode, "channel", p.ChannelID, "why", why)
		return "", nil
	}
	return sent.ID, nil
}

// recent is how many of the channel's latest messages Find reads: Discord's
// most for one request.
const recent = 50

// Find looks for c among the channel's latest 50 messages, for a card whose
// post may have been cut short after the send (paddock#67, R3). Only the bot's
// own messages count, and only their rich embeds: a message matches when one
// links to c's URL, or, for a card with no link, has no link and c's title and
// timestamp. It returns the message's id and true when one matches. Reading
// the channel needs the bot's Read Message History permission there; without
// it Find returns a StatusError (403).
func (p *Poster) Find(ctx context.Context, c card.Card) (string, bool, error) {
	if p.Token == "" || p.ChannelID == "" {
		return "", false, errors.New("discord: reading the channel needs a bot token and a channel id")
	}
	me, err := p.self(ctx)
	if err != nil {
		return "", false, err
	}
	data, err := p.get(ctx, "/channels/"+p.ChannelID+"/messages?limit="+strconv.Itoa(recent))
	if err != nil {
		return "", false, err
	}
	var messages []struct {
		ID     string `json:"id"`
		Author struct {
			ID string `json:"id"`
		} `json:"author"`
		Embeds []Embed `json:"embeds"`
	}
	if err := json.Unmarshal(data, &messages); err != nil {
		return "", false, fmt.Errorf("discord: the channel's messages are unreadable: %w", err)
	}
	want := EmbedOf(c)
	for _, m := range messages {
		if m.Author.ID != me {
			continue
		}
		for _, e := range m.Embeds {
			if e.Type == "rich" && same(e, want) {
				return m.ID, true, nil
			}
		}
	}
	return "", false, nil
}

// self is the bot's own user id, read from GET /users/@me the first time it is
// asked for and kept.
func (p *Poster) self(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.me != "" {
		return p.me, nil
	}
	data, err := p.get(ctx, "/users/@me")
	if err != nil {
		return "", err
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &user); err != nil || user.ID == "" {
		return "", errors.New("discord: the bot's own user is unreadable")
	}
	p.me = user.ID
	return p.me, nil
}

// get is a GET of path under the API, as the bot: the body of a success, or a
// StatusError.
func (p *Poster) get(ctx context.Context, path string) ([]byte, error) {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+p.Token)
	req.Header.Set("User-Agent", userAgent)
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 50 messages with their embeds are well under a megabyte.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), maxBody)]))}
		if resp.StatusCode == http.StatusTooManyRequests {
			e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), data)
		}
		return nil, e
	}
	if err != nil {
		return nil, fmt.Errorf("discord: reading %s: %w", path, err)
	}
	return data, nil
}

// discordEpoch is the first second of 2015, from which a snowflake counts its
// milliseconds.
const discordEpoch = 1420070400000

// Created is when the message with id was created, read from the id itself (a
// snowflake: its top 42 bits are milliseconds since 2015), and false when id
// isn't one.
func Created(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n>>22) + discordEpoch).UTC(), true //nolint:gosec // 42 bits: no overflow
}

// same is whether embed e, as Discord returns it, is the card want: the same
// link, or with no link the same title and timestamp. Discord may write the
// timestamp in another form (+00:00 for Z), so it is compared as a time.
func same(e, want Embed) bool {
	if want.URL != "" {
		return e.URL == want.URL
	}
	if e.URL != "" || e.Title != want.Title {
		return false
	}
	if want.Timestamp == "" {
		return e.Timestamp == ""
	}
	got, err1 := time.Parse(time.RFC3339Nano, e.Timestamp)
	wanted, err2 := time.Parse(time.RFC3339Nano, want.Timestamp)
	return err1 == nil && err2 == nil && got.Equal(wanted)
}

// retryAfter is the wait a 429 asks for: the Retry-After header's seconds, else
// the JSON body's retry_after (seconds, with a fraction). Zero when neither
// reads as a wait.
func retryAfter(header string, body []byte) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil {
		var limited struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(body, &limited) != nil {
			return 0
		}
		secs = limited.RetryAfter
	}
	if !(secs > 0) || math.IsInf(secs, 0) || secs > 24*3600 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

type message struct {
	Embeds          []Embed         `json:"embeds"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
	Nonce           string          `json:"nonce,omitempty"`
	EnforceNonce    bool            `json:"enforce_nonce,omitempty"`
}

// allowedMentions with nothing to parse: a card never pings anyone, whatever a
// car's name holds.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

// Embed is Discord's embed object, the fields a card fills.
type Embed struct {
	// Type is the embed's type as Discord returns it: "rich" for an embed a
	// bot sent. Left out when sending, where rich is the default.
	Type        string       `json:"type,omitempty"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      EmbedFooter  `json:"footer"`
}

// EmbedField is one of an embed's fields.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// EmbedFooter is an embed's footer.
type EmbedFooter struct {
	Text string `json:"text"`
}

// EmbedOf is c as Discord's embed object, as v1's results.to_embed made it: the
// title linking to the card's URL, the result as the description, every field
// inline, the colour, the footer and the start as the timestamp, which Discord
// shows in each viewer's own time.
func EmbedOf(c card.Card) Embed {
	e := Embed{Title: c.Title, Description: c.Description, URL: c.URL, Color: c.Colour, Footer: EmbedFooter{c.Footer}}
	if !c.Timestamp.IsZero() {
		e.Timestamp = c.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	for _, f := range c.Fields {
		e.Fields = append(e.Fields, EmbedField{Name: f.Name, Value: f.Value, Inline: true})
	}
	return e
}
