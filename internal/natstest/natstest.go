// Package natstest runs a real nats-server for the tests, with paddock's EVENTS
// stream and cards bucket: nothing of NATS is mocked (paddock#34, R9), as in
// ecu and logbook. Only tests import it.
//
// It runs the nats-server release paddock pins (scripts/nats_it.sh, nats:2.15.1)
// as a process of its own, as ecu's internal/bus/bustest does. The release
// archive is downloaded once into the user's cache and checked against these
// sums, from the release's SHA256SUMS. TWR_TEST_NATS_SERVER names a binary to
// use instead, which must be this release.
package natstest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsVersion is the nats-server release the tests run.
const natsVersion = "v2.15.1"

var natsSHA256 = map[string]string{
	"darwin-amd64": "3cba5976a4fbd022669d3401cab4147faeb2b4c079712c882edb9ae4e52feb06",
	"darwin-arm64": "ed0fe39222ec2e317c4d0e1a9cdc16c63ec523d94f9355d24a902a4db2655ffd",
	"linux-amd64":  "bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70",
	"linux-arm64":  "e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c",
}

// releases is where the releases are downloaded from: a var, so a test can
// serve one.
var releases = "https://github.com/nats-io/nats-server/releases/download/"

var (
	binaryOnce sync.Once
	binary     string
	binaryErr  error
)

func natsBinary() (string, error) {
	binaryOnce.Do(func() { binary, binaryErr = findNATSServer(os.Getenv("TWR_TEST_NATS_SERVER")) })
	return binary, binaryErr
}

// findNATSServer is bin, when set, or the pinned release from the cache or
// GitHub.
func findNATSServer(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if bin != "" {
		out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // the binary the person running the tests named
		if err != nil {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s: %w", bin, err)
		}
		if !strings.Contains(string(out), natsVersion) {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s is %q, not %s", bin, strings.TrimSpace(string(out)), natsVersion)
		}
		return bin, nil
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	sum, ok := natsSHA256[platform]
	if !ok {
		return "", fmt.Errorf("no %s release for %s pinned here; set TWR_TEST_NATS_SERVER", natsVersion, platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	name := "nats-server-" + natsVersion + "-" + platform
	dir := filepath.Join(cache, "those-who-run", name)
	bin = filepath.Join(dir, "nats-server")
	archivePath := filepath.Join(dir, "nats-server.tar.gz")
	// The archive is kept beside the binary and checked on every use, and the
	// binary taken from it again: a cache someone else can write to is not
	// trusted as it stands.
	if archive, err := os.ReadFile(archivePath); err == nil { //nolint:gosec // the tests' own cache
		if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) == sum {
			return bin, extract(archive, name+"/nats-server", bin)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	url := releases + natsVersion + "/" + name + ".tar.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != sum {
		return "", fmt.Errorf("%s has sha256 %x, want %s", url, got, sum)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return "", err
	}
	return bin, extract(archive, name+"/nats-server", bin)
}

// extract writes the archive's file member to path, through a temporary file
// renamed into place, so a run cut short leaves no half binary for the next.
func extract(archive []byte, member, path string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s is not in the archive", member)
		}
		if err != nil {
			return err
		}
		if h.Name != member {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".nats-server-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if _, err := io.Copy(tmp, io.LimitReader(tr, 128<<20)); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := errors.Join(tmp.Chmod(0o755), tmp.Close()); err != nil { //nolint:gosec // an executable
			return err
		}
		return os.Rename(tmp.Name(), path)
	}
}

// defs are paddock's deploy/nats/streams/events.json and kv/cards.json at
// 489b08b (paddock#332), copied unchanged: the definitions the bootstrap
// applies in compose and the cluster.
//
//go:embed nats/*.json
var defs embed.FS

// Server is a nats-server -js the test started.
type Server struct {
	// URL is the server's address for a client.
	URL string
	// JS is a JetStream context on it, closed when the test ends.
	JS jetstream.JetStream
}

// Start starts nats-server -js on a free port, with EVENTS and the cards
// bucket, stopped when the test ends.
func Start(t testing.TB) (jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	s := Bare(t)
	var stream jetstream.StreamConfig
	readDef(t, "events.json", &stream)
	if _, err := s.JS.CreateStream(context.Background(), stream); err != nil {
		t.Fatalf("EVENTS: %v", err)
	}
	var def struct {
		Bucket        string `json:"bucket"`
		Description   string `json:"description"`
		History       uint8  `json:"history"`
		Replicas      int    `json:"replicas"`
		MaxValueSize  int32  `json:"max_value_size"`
		MaxBucketSize int64  `json:"max_bucket_size"`
	}
	readDef(t, "cards.json", &def)
	kv, err := s.JS.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket: def.Bucket, Description: def.Description, History: def.History, Storage: jetstream.FileStorage,
		Replicas: def.Replicas, MaxValueSize: def.MaxValueSize, MaxBytes: def.MaxBucketSize,
	})
	if err != nil {
		t.Fatalf("cards: %v", err)
	}
	return s.JS, kv
}

// StartURL is Start, with the server's URL, for a test of a client that
// connects itself.
func StartURL(t testing.TB) (string, jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	js, kv := Start(t)
	return js.Conn().ConnectedUrl(), js, kv
}

// Bare starts nats-server -js on a free port with no stream or bucket, stopped
// when the test ends.
func Bare(t testing.TB) Server {
	t.Helper()
	bin, err := natsBinary()
	if err != nil {
		t.Fatalf("nats-server: %v", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cmd := exec.Command(bin, "-a", "127.0.0.1", "-p", strconv.Itoa(port), "-js", "-sd", t.TempDir()) //nolint:gosec // the pinned nats-server
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	url := "nats://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(10 * time.Second)
	var nc *nats.Conn
	for {
		if nc, err = nats.Connect(url); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("nats-server stopped: %s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server not ready after 10 s: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return Server{URL: url, JS: js}
}

func readDef(t testing.TB, name string, v any) {
	t.Helper()
	data, err := defs.ReadFile("nats/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
