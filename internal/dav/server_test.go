package dav

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// fakeBackend stands in for 115. It serves listings from a fixed tree and
// hands out URLs to an httptest origin that behaves like the CDN, including
// refusing links from an earlier generation.
type fakeBackend struct {
	t     *testing.T
	dirs  map[string][]pan115.Entry
	blobs map[string][]byte

	origin *httptest.Server

	// generation models link expiry: bumping it invalidates every URL the
	// backend has handed out so far.
	generation atomic.Int64

	listCalls    atomic.Int64
	resolveCalls atomic.Int64
	fetches      atomic.Int64
	rejections   atomic.Int64
}

const (
	fakeUserAgent = "Mozilla/5.0 115Browser/27.0.5.7"
	fakeCookie    = "UID=u; CID=c; SEID=s"
)

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	b := &fakeBackend{t: t, dirs: map[string][]pan115.Entry{}, blobs: map[string][]byte{}}
	b.generation.Store(1)

	b.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.fetches.Add(1)

		// The CDN checks the identity that asked for the URL; so do we, since
		// getting that wrong is the failure this whole design exists to avoid.
		if got := r.Header.Get("User-Agent"); got != fakeUserAgent {
			b.t.Errorf("origin saw User-Agent %q, want %q", got, fakeUserAgent)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "UID=u") {
			b.t.Errorf("origin saw Cookie %q, want the 115 session", got)
		}

		if r.URL.Query().Get("g") != fmt.Sprint(b.generation.Load()) {
			b.rejections.Add(1)
			http.Error(w, "link expired", http.StatusForbidden)
			return
		}
		blob, ok := b.blobs[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Unix(0, 0), strings.NewReader(string(blob)))
	}))
	t.Cleanup(b.origin.Close)
	return b
}

func (b *fakeBackend) List(_ context.Context, id string) ([]pan115.Entry, error) {
	b.listCalls.Add(1)
	entries, ok := b.dirs[id]
	if !ok {
		return nil, pan115.ErrNotFound
	}
	return entries, nil
}

func (b *fakeBackend) Resolve(_ context.Context, pickCode string) (*pan115.Target, error) {
	b.resolveCalls.Add(1)
	if _, ok := b.blobs[pickCode]; !ok {
		return nil, &pan115.ErrNotDownloadable{PickCode: pickCode}
	}
	header := http.Header{}
	header.Set("User-Agent", fakeUserAgent)
	header.Set("Cookie", fakeCookie)
	return &pan115.Target{
		URL:    fmt.Sprintf("%s/%s?g=%d", b.origin.URL, pickCode, b.generation.Load()),
		Size:   int64(len(b.blobs[pickCode])),
		Header: header,
	}, nil
}

// sample builds a small account: one film at the root and one in a subdirectory.
func sample(t *testing.T) *fakeBackend {
	t.Helper()
	b := newFakeBackend(t)
	b.blobs["pc-film"] = []byte(strings.Repeat("film-payload-", 800))
	b.blobs["pc-nested"] = []byte("nested payload")

	b.dirs["0"] = []pan115.Entry{
		{ID: "d1", Name: "Movies", IsDir: true, ModTime: time.Unix(1700000000, 0)},
		{ID: "f1", Name: "film.mkv", Size: int64(len(b.blobs["pc-film"])), PickCode: "pc-film",
			SHA1: "abc123", ModTime: time.Unix(1700000100, 0)},
	}
	b.dirs["d1"] = []pan115.Entry{
		{ID: "f2", Name: "nested.mp4", Size: int64(len(b.blobs["pc-nested"])), PickCode: "pc-nested",
			ModTime: time.Unix(1700000200, 0)},
	}
	return b
}

func newTestServer(t *testing.T, b *fakeBackend, opts Options) *httptest.Server {
	t.Helper()
	opts.Backend = b
	if opts.DirTTL == 0 {
		opts.DirTTL = time.Minute
	}
	if opts.LinkTTL == 0 {
		opts.LinkTTL = time.Hour
	}
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := httptest.NewServer(New(opts))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGetWholeFile(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := body(t, resp), string(b.blobs["pc-film"]); got != want {
		t.Errorf("body length %d, want %d", len(got), len(want))
	}
	if got := resp.Header.Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q, want video/x-matroska", got)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

// Seeking is the behaviour Infuse depends on most, and the reason bytes are
// proxied rather than redirected.
func TestRangeRequestIsPassedThrough(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})
	blob := b.blobs["pc-film"]

	resp := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{
		"Range": "bytes=100-199",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got, want := body(t, resp), string(blob[100:200]); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	wantRange := fmt.Sprintf("bytes 100-199/%d", len(blob))
	if got := resp.Header.Get("Content-Range"); got != wantRange {
		t.Errorf("Content-Range = %q, want %q", got, wantRange)
	}
}

func TestOpenEndedRangeFromMiddle(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})
	blob := b.blobs["pc-film"]
	offset := len(blob) - 32

	resp := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{
		"Range": fmt.Sprintf("bytes=%d-", offset),
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got, want := body(t, resp), string(blob[offset:]); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// HEAD is answered from the listing, so it must not cost an API round trip.
func TestHeadUsesMetadataOnly(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodHead, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.ContentLength, int64(len(b.blobs["pc-film"])); got != want {
		t.Errorf("Content-Length = %d, want %d", got, want)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if n := b.resolveCalls.Load(); n != 0 {
		t.Errorf("HEAD triggered %d resolve calls, want 0", n)
	}
	if n := b.fetches.Load(); n != 0 {
		t.Errorf("HEAD reached the origin %d times, want 0", n)
	}
}

// A cached URL that the CDN has started refusing must be re-resolved
// transparently, without the player seeing an error.
func TestExpiredLinkIsResolvedAgain(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	if resp := do(t, srv, http.MethodGet, "/film.mkv", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200", resp.StatusCode)
	}
	resolvesBefore := b.resolveCalls.Load()

	// Every URL handed out so far is now stale.
	b.generation.Add(1)

	resp := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{"Range": "bytes=0-9"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 after re-resolving", resp.StatusCode)
	}
	if got, want := body(t, resp), string(b.blobs["pc-film"][:10]); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if b.rejections.Load() == 0 {
		t.Error("expected the origin to reject the stale link at least once")
	}
	if got := b.resolveCalls.Load(); got <= resolvesBefore {
		t.Errorf("resolve calls = %d, want more than %d", got, resolvesBefore)
	}
}

func TestNestedPathAndListingCache(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodGet, "/Movies/nested.mp4", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := body(t, resp), string(b.blobs["pc-nested"]); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	// Walking to /Movies/nested.mp4 lists the root and Movies once each.
	afterFirst := b.listCalls.Load()
	if afterFirst != 2 {
		t.Fatalf("list calls = %d, want 2", afterFirst)
	}
	do(t, srv, http.MethodGet, "/Movies/nested.mp4", nil)
	if got := b.listCalls.Load(); got != afterFirst {
		t.Errorf("list calls = %d after a cached lookup, want %d", got, afterFirst)
	}
}

func TestMissingPathIs404(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})
	for _, path := range []string{"/nope.mkv", "/Movies/nope.mkv", "/nope/deep.mkv"} {
		if resp := do(t, srv, http.MethodGet, path, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestPropfindListsChildren(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	xml := body(t, resp)
	for _, want := range []string{"film.mkv", "Movies", "video/x-matroska", "sha1:abc123"} {
		if !strings.Contains(xml, want) {
			t.Errorf("multistatus is missing %q\n%s", want, xml)
		}
	}
}

func TestWriteMethodsAreRefused(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "UNLOCK", "POST"} {
		resp := do(t, srv, method, "/film.mkv", nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != allowedMethods {
			t.Errorf("%s: Allow = %q, want %q", method, got, allowedMethods)
		}
	}
}

func TestOptionsAdvertisesReadOnly(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})
	resp := do(t, srv, http.MethodOptions, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("DAV"); got == "" {
		t.Error("OPTIONS did not advertise DAV support")
	}
	if got := resp.Header.Get("Allow"); got != allowedMethods {
		t.Errorf("Allow = %q, want %q", got, allowedMethods)
	}
}

func TestBasicAuth(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{Username: "u", Password: "p"})

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 did not carry a WWW-Authenticate challenge")
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/film.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("u", "p")
	authed, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Errorf("authenticated status = %d, want 200", authed.StatusCode)
	}
}

func TestUndownloadableFileIsForbidden(t *testing.T) {
	b := sample(t)
	b.dirs["0"] = append(b.dirs["0"], pan115.Entry{
		ID: "f9", Name: "blocked.mkv", Size: 10, PickCode: "pc-missing",
	})
	srv := newTestServer(t, b, Options{})

	if resp := do(t, srv, http.MethodGet, "/blocked.mkv", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestDirectoryIndex(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	for _, want := range []string{"film.mkv", "Movies/"} {
		if !strings.Contains(page, want) {
			t.Errorf("index is missing %q\n%s", want, page)
		}
	}
}
