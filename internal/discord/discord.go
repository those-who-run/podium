// Package discord posts results cards to a Discord text channel over Discord's
// REST API, "Create Message" with an embed, authenticated with a bot token
// (paddock ADR 0010, point 2). It opens no gateway session and needs no
// privileged intents: the bot needs View Channel, Send Messages and Embed Links
// on the channel, as v1's did.
//
// A Poster sends once. Retries, and posting each card exactly once, are its
// caller's (paddock#67); StatusError's Permanent says which failures a retry
// cannot fix.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
}

// StatusError is a response from Discord other than a success.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the start of the response's body: Discord's JSON error.
	Body string
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
	_, err := p.Send(ctx, c)
	return err
}

// Send sends c to the channel and returns the message's id.
func (p *Poster) Send(ctx context.Context, c card.Card) (string, error) {
	if p.Token == "" || p.ChannelID == "" {
		return "", errors.New("discord: posting needs a bot token and a channel id")
	}
	body, err := json.Marshal(message{Embeds: []Embed{EmbedOf(c)}, AllowedMentions: allowedMentions{Parse: []string{}}})
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("discord: reading the response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	var sent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &sent); err != nil {
		return "", fmt.Errorf("discord: the message sent, its id unreadable: %w", err)
	}
	return sent.ID, nil
}

type message struct {
	Embeds          []Embed         `json:"embeds"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// allowedMentions with nothing to parse: a card never pings anyone, whatever a
// car's name holds.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

// Embed is Discord's embed object, the fields a card fills.
type Embed struct {
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
