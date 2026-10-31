// Package service is podium's service, kept to the conventions every Go component
// starts from (README.md, "Conventions"): configuration from the environment,
// log/slog writing JSON, a health endpoint and its metrics. With cards on, it
// posts each session's results card from the bus (internal/cards, paddock#67).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/podium/internal/cards"
	"github.com/those-who-run/podium/internal/discord"
	"github.com/those-who-run/podium/internal/sink"
)

// Config is the service's configuration, read from the environment by
// ConfigFromEnv.
type Config struct {
	// Addr is the address the HTTP server listens on: ADDR, ":8080" when unset.
	Addr string
	// LogLevel is the lowest level logged: LOG_LEVEL (debug, info, warn or error),
	// info when unset.
	LogLevel slog.Level
	// Cards is whether the cards are posted: always with the console sink, and
	// with the discord sink only when a results channel is set, as in v1, where
	// an unset channel turned cards off (paddock#67, R7).
	Cards bool
	// Sink is where the cards go, TWR_PODIUM_SINK: discord (the default) or
	// console, which writes them to the log's stream for replays and tests.
	Sink string
	// Src is the source whose sessions it posts, TWR_PODIUM_SRC: live unless
	// set, or a replay's.
	Src string
	// NATSURL is the NATS server, TWR_NATS_URL; needed when cards are on.
	NATSURL string
	// ChannelID is the results channel, TWR_DISCORD_RESULTS_CHANNEL_ID.
	ChannelID string
	// Token is the bot's token, TWR_DISCORD_BOT_TOKEN, radio's name for the
	// shared Secret (paddock ADR 0010, point 2). It is never logged.
	Token string
	// Pitwall is the pit wall's base URL each card links to, TWR_PITWALL_URL;
	// empty is no link.
	Pitwall string
}

// The sinks TWR_PODIUM_SINK names.
const (
	SinkDiscord = "discord"
	SinkConsole = "console"
)

// ConfigFromEnv reads the configuration through getenv, which is os.Getenv outside
// the tests. An unset or empty variable takes its default; a value that can't be
// read is an error, so a typo stops the service rather than being ignored.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: ":8080", LogLevel: slog.LevelInfo}
	if addr := getenv("ADDR"); addr != "" {
		cfg.Addr = addr
	}
	if level := getenv("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	cfg.Sink, cfg.Src = SinkDiscord, "live"
	if s := getenv("TWR_PODIUM_SINK"); s != "" {
		cfg.Sink = s
	}
	if s := getenv("TWR_PODIUM_SRC"); s != "" {
		cfg.Src = s
	}
	cfg.NATSURL = getenv("TWR_NATS_URL")
	cfg.ChannelID = getenv("TWR_DISCORD_RESULTS_CHANNEL_ID")
	cfg.Token = getenv("TWR_DISCORD_BOT_TOKEN")
	cfg.Pitwall = getenv("TWR_PITWALL_URL")
	switch cfg.Sink {
	case SinkConsole:
		cfg.Cards = true
	case SinkDiscord:
		cfg.Cards = cfg.ChannelID != ""
		if cfg.Cards && cfg.Token == "" {
			return Config{}, errors.New("TWR_DISCORD_BOT_TOKEN: posting to TWR_DISCORD_RESULTS_CHANNEL_ID needs the bot's token")
		}
	default:
		return Config{}, fmt.Errorf("TWR_PODIUM_SINK: %q is not discord or console", cfg.Sink)
	}
	if cfg.Cards && cfg.NATSURL == "" {
		return Config{}, errors.New("TWR_NATS_URL: posting cards needs the NATS server")
	}
	return cfg, nil
}

// NewLogger returns a logger that writes JSON lines to w, one object per record,
// from level up.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Handler serves the health endpoint, GET /healthz, which answers 200 with the
// service's version while the process can serve requests, and GET /metrics,
// the cards' counter in Prometheus's text format (paddock#67, R5). With cards
// on, /healthz answers 503 until NATS's first connection, so the pod isn't
// Ready on a connection it hasn't made (paddock#340); ready says whether it has
// been made, and nil is always.
func Handler(version string, counters *cards.Counters, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		if ready != nil && !ready() {
			status = "waiting for NATS"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		// An error here is the client gone; there is no one left to tell.
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "version": version})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = counters.WriteTo(w)
	})
	return mux
}

// postGrace is how long Run waits, once the server has stopped, for a card in
// flight to be recorded before it closes the NATS connection.
const postGrace = 5 * time.Second

// shutdownGrace is how long Run lets requests in flight finish once ctx is done.
const shutdownGrace = 10 * time.Second

// The server's limits, for a small JSON service whose requests are short: a client
// that is slow to send its headers or its whole request, or slow to read the
// response, is cut off rather than holding a connection, and an idle keep-alive
// connection is closed after a minute. A component with long requests or
// streaming responses raises them.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// Run listens on cfg.Addr and serves Handler until ctx is done, then shuts the
// server down gracefully, posting the cards meanwhile when cfg.Cards is set. It
// returns nil after a clean shutdown, or why it could not listen, serve or
// post.
func Run(ctx context.Context, cfg Config, logger *slog.Logger, version string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	counters := &cards.Counters{}
	if !cfg.Cards {
		// R7: no results channel, no consumer.
		logger.InfoContext(ctx, "cards off: TWR_DISCORD_RESULTS_CHANNEL_ID is not set")
		return serve(ctx, ln, logger, version, counters, nil)
	}
	// NATS not up yet at start is waited for, not a restart (paddock#340): Connect
	// returns before the first connection is made, the client keeps trying, and
	// the cards start once it is made.
	first := newLatch()
	nc, err := nats.Connect(cfg.NATSURL, nats.Name("podium"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.ConnectHandler(func(*nats.Conn) { first.set() }))
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("nats: %w", err)
	}
	defer nc.Close()
	if nc.IsConnected() {
		first.set()
	} else {
		logger.InfoContext(ctx, "waiting for NATS's first connection")
	}
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	posting := make(chan error, 1)
	go func() {
		select {
		case <-first.done:
		case <-ctx.Done():
			posting <- nil
			return
		}
		started, err := startCards(ctx, nc, cfg, logger, counters)
		if err != nil {
			posting <- err
			return
		}
		posting <- <-started
	}()
	posterDone := make(chan struct{})
	go func() {
		defer close(posterDone)
		if err := <-posting; err != nil {
			cancel(err)
		}
	}()
	err = serve(ctx, ln, logger, version, counters, first.isSet)
	if err != nil {
		// Stop the poster too, which a failed server leaves running.
		cancel(err)
	}
	// Let a card in flight finish its record before the connection closes.
	select {
	case <-posterDone:
	case <-time.After(postGrace):
		logger.WarnContext(ctx, "cards: stopped with a card in flight")
	}
	if err != nil {
		return err
	}
	if parent.Err() != nil {
		// Stopped from outside: a clean stop.
		return nil
	}
	// The poster stopped the service.
	return context.Cause(ctx)
}

// startCards starts posting the cards (internal/cards) on nc, and returns
// where the poster's error arrives when it stops.
func startCards(ctx context.Context, nc *nats.Conn, cfg Config, logger *slog.Logger,
	counters *cards.Counters,
) (<-chan error, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.KeyValue(ctx, cards.Bucket)
	if err != nil {
		return nil, fmt.Errorf("KV bucket %s: %w", cards.Bucket, err)
	}
	var sender cards.Sender = sink.NewConsole(logWriter{logger})
	if cfg.Sink == SinkDiscord {
		sender = &discord.Poster{Token: cfg.Token, ChannelID: cfg.ChannelID, Logger: logger}
	}
	p := &cards.Poster{
		JS: js, KV: kv, Src: cfg.Src, Sender: sender, Pitwall: cfg.Pitwall,
		Counters: counters, Logger: logger,
	}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	return done, nil
}

// latch is set once and stays set: here, NATS's first connection.
type latch struct {
	once sync.Once
	done chan struct{}
}

func newLatch() *latch { return &latch{done: make(chan struct{})} }

func (l *latch) set() { l.once.Do(func() { close(l.done) }) }

func (l *latch) isSet() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// logWriter writes the console sink's cards as log lines, so the service's
// output stays one JSON object a line.
type logWriter struct{ logger *slog.Logger }

func (w logWriter) Write(b []byte) (int, error) {
	w.logger.Info("card", "text", string(b)) //nolint:sloglint // no context: an io.Writer
	return len(b), nil
}

// newServer is the HTTP server for h, with the limits above, logging its own errors
// through logger.
func newServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

func serve(ctx context.Context, ln net.Listener, logger *slog.Logger, version string, counters *cards.Counters,
	ready func() bool,
) error {
	srv := newServer(Handler(version, counters, ready), logger)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "serving", "addr", ln.Addr().String(), "version", version)

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	logger.InfoContext(ctx, "shutting down")
	// ctx is already done, so the grace period needs a context of its own.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	return shutdown(grace, srv, served)
}

// shutdown stops srv gracefully within ctx and waits for its Serve to return the
// error that served carries. Once Shutdown is called Serve returns
// http.ErrServerClosed, which is a clean stop. Anything else is an error Serve hit
// on its own, as ctx was done, and is not lost: it is returned with any error from
// Shutdown.
func shutdown(ctx context.Context, srv *http.Server, served <-chan error) error {
	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, fmt.Errorf("serve: %w", err))
	}
	return errors.Join(errs...)
}
