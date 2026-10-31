package natstest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Start makes EVENTS and the cards bucket as paddock defines them.
func TestStart(t *testing.T) {
	url, js, kv := StartURL(t)
	st, err := js.Stream(t.Context(), "EVENTS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(t.Context())
	if err != nil || len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != "twr.v1.*.evt.>" {
		t.Errorf("EVENTS is %v, %v", info, err)
	}
	status, err := kv.Status(t.Context())
	if err != nil || status.Bucket() != "cards" || status.History() != 1 || url == "" {
		t.Errorf("cards is %v, %v; url %q", status, err, url)
	}
}

// A Later starts the server, with EVENTS and the cards bucket, at the URL
// FreeURL gave before any server was there.
func TestStartLaterAtAFreeURL(t *testing.T) {
	later := FreeURL(t)
	js, kv := later.Start(t)
	if got := js.Conn().ConnectedUrl(); got != later.URL {
		t.Errorf("connected to %q, want %q", got, later.URL)
	}
	if _, err := js.Stream(t.Context(), "EVENTS"); err != nil {
		t.Errorf("EVENTS: %v", err)
	}
	if kv.Bucket() != "cards" {
		t.Errorf("bucket %q, want cards", kv.Bucket())
	}
}

func TestANamedServerMustBeThePinnedRelease(t *testing.T) {
	if _, err := findNATSServer(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a binary that isn't there is no error")
	}
	script := filepath.Join(t.TempDir(), "nats-server")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho nats-server: v2.0.0\n"), 0o700); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	if _, err := findNATSServer(script); err == nil {
		t.Error("another release is no error")
	}
}

func TestExtractRefusesWhatIsNotTheBinary(t *testing.T) {
	if err := extract([]byte("not gzip"), "x", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("not gzip is no error")
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, _ = gz.Write(make([]byte, 1024))
	_ = gz.Close()
	if err := extract(b.Bytes(), "x", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("an archive without the member is no error")
	}
}

// A release is downloaded into an empty cache, checked against its sum, and
// taken from the cache the next time; one with another sum is refused.
func TestTheReleaseIsDownloadedAndChecked(t *testing.T) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if _, ok := natsSHA256[platform]; !ok {
		t.Fatalf("no release pinned for %s", platform)
	}
	name := "nats-server-" + natsVersion + "-" + platform
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\n")
	_ = tw.WriteHeader(&tar.Header{Name: name + "/nats-server", Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	archive := b.Bytes()
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		if !strings.HasSuffix(r.URL.Path, "/"+natsVersion+"/"+name+".tar.gz") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	oldURL, oldSum := releases, natsSHA256[platform]
	t.Cleanup(func() { releases, natsSHA256[platform] = oldURL, oldSum })
	releases = srv.URL + "/"
	sum := sha256.Sum256(archive)
	natsSHA256[platform] = hex.EncodeToString(sum[:])
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	bin, err := findNATSServer("")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(bin); err != nil || !bytes.Equal(got, body) { //nolint:gosec // the test's own file
		t.Errorf("the binary is %q, %v", got, err)
	}
	if _, err := findNATSServer(""); err != nil || served != 1 {
		t.Errorf("the second find: %v after %d downloads, want the cache", err, served)
	}
	natsSHA256[platform] = strings.Repeat("0", 64)
	if _, err := findNATSServer(""); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("another sum: %v", err)
	}
	releases = srv.URL + "/missing/"
	if _, err := findNATSServer(""); err == nil {
		t.Error("a release that isn't there is no error")
	}
}
