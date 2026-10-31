package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"

	"github.com/those-who-run/podium/internal/card"
	"github.com/those-who-run/podium/internal/natstest"
)

// paddock#67: with the console sink, the service posts a session's card from
// the bus to its log and counts it on /metrics, then stops cleanly.
func TestRunPostsTheCardsFromTheBus(t *testing.T) {
	url, js, _ := natstest.StartURL(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := newReadyLog()
	cfg := Config{Addr: "127.0.0.1:0", Cards: true, Sink: SinkConsole, Src: "replay-svc", NATSURL: url, Pitwall: "http://pw"}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, NewLogger(logs, slog.LevelInfo), "test") }()
	waitServing(t, logs, done)
	postsACard(t, js, logs)
	body, _ := getBody(t, "http://"+logs.addr()+"/metrics")
	if !strings.Contains(body, `gt7_results_posts_total{result="ok"} 1`) {
		t.Errorf("/metrics:\n%s", body)
	}
	stops(t, cancel, done)
}

// paddock#340: podium started before its NATS server waits for it, answering
// /healthz 503 until the first connection, and posts the card once the server
// is up, rather than stopping.
func TestRunWaitsForANATSThatIsNotUpYet(t *testing.T) {
	url := natstest.FreeURL(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := newReadyLog()
	cfg := Config{Addr: "127.0.0.1:0", Cards: true, Sink: SinkConsole, Src: "replay-svc", NATSURL: url, Pitwall: "http://pw"}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, NewLogger(logs, slog.LevelInfo), "test") }()
	waitServing(t, logs, done)
	if body, code := getBody(t, "http://"+logs.addr()+"/healthz"); code != http.StatusServiceUnavailable ||
		!strings.Contains(body, "waiting for NATS") {
		t.Errorf("/healthz before NATS is %d %s, want 503 waiting for NATS", code, body)
	}
	// Longer than the client's first attempts: still waiting, not stopped.
	select {
	case err := <-done:
		t.Fatalf("Run = %v with NATS not up yet, want it waiting; log %s", err, logs.String())
	case <-time.After(500 * time.Millisecond):
	}

	js, _ := natstest.StartAt(t, url)
	postsACard(t, js, logs)
	if body, code := getBody(t, "http://"+logs.addr()+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz once connected is %d %s, want 200", code, body)
	}
	stops(t, cancel, done)
}

// waitServing waits for Run, started with logs and done, to serve.
func waitServing(t *testing.T, logs *readyLog, done <-chan error) {
	t.Helper()
	select {
	case <-logs.ready:
	case err := <-done:
		t.Fatalf("Run = %v before serving, log %q", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
}

// postsACard publishes Fuji's session.ended for replay-svc once the service's
// consumer exists, and waits for its card in logs.
func postsACard(t *testing.T, js jetstream.JetStream, logs *readyLog) {
	t.Helper()
	f, err := os.Open("../card/testdata/ended/real-nuc-fuji-timed-2026-10-15.jsonl") //nolint:gosec // a fixed test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ended, err := card.ReadEnded(f)
	if err != nil {
		t.Fatal(err)
	}
	e := ended[0]
	// The consumer posts only what ends once it exists.
	for made := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := js.Consumer(t.Context(), "EVENTS", "cards-replay-svc"); err == nil {
			break
		}
		if time.Now().After(made) {
			t.Fatalf("no consumer after 15 s:\n%s", logs.String())
		}
	}
	h, data, err := ce.Encode(e, ce.Event{ID: "replay-svc:" + e.GetSessionId() + ":session.ended:1", Source: "/twr/ecu/gt7", Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.PublishMsg(t.Context(), &nats.Msg{
		Subject: "twr.v1.replay-svc.evt." + e.GetSessionId() + ".session.ended", Header: nats.Header(h), Data: data,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(logs.String(), `"msg":"card posted"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no card posted after 15 s:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "Fuji International Speedway · time trial") {
		t.Errorf("the card is not in the log:\n%s", logs.String())
	}
}

// getBody GETs url and returns its body and status.
func getBody(t *testing.T, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(body), resp.StatusCode
}

// stops cancels Run and checks it stops cleanly.
func stops(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run still serving 10 s after cancel")
	}
}

// A server without the cards bucket (no bootstrap) stops the service.
func TestRunNeedsTheCardsBucket(t *testing.T) {
	s := natstest.Bare(t)
	cfg := Config{Addr: "127.0.0.1:0", Cards: true, Sink: SinkDiscord, ChannelID: "1", Token: "t", Src: "live", NATSURL: s.URL}
	err := Run(t.Context(), cfg, NewLogger(io.Discard, slog.LevelInfo), "test")
	if err == nil || !strings.Contains(err.Error(), "KV bucket cards") {
		t.Errorf("Run = %v, want the bucket's error", err)
	}
}

// A poster that can't consume (no EVENTS stream) stops the service with its
// error.
func TestRunStopsWhenThePosterCannot(t *testing.T) {
	s := natstest.Bare(t)
	if _, err := s.JS.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cards"}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Addr: "127.0.0.1:0", Cards: true, Sink: SinkConsole, Src: "live", NATSURL: s.URL}
	err := Run(t.Context(), cfg, NewLogger(io.Discard, slog.LevelInfo), "test")
	if err == nil || !strings.Contains(err.Error(), "consumer cards-live") {
		t.Errorf("Run = %v, want the consumer's error", err)
	}
}
