package dav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	listErr       atomic.Pointer[error]
	resolveErr    atomic.Pointer[error]
	dropOnce      atomic.Bool
	beforeList    func()
	beforeResolve func()
	listCalls     atomic.Int64
	resolveCalls  atomic.Int64
	fetches       atomic.Int64
	rejections    atomic.Int64
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

		// Drop the connection without answering, the way a flaky uplink does.
		if b.dropOnce.CompareAndSwap(true, false) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}

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

func (b *fakeBackend) List(ctx context.Context, id string) ([]pan115.Entry, error) {
	b.listCalls.Add(1)
	if b.beforeList != nil {
		b.beforeList()
	}
	// A real client aborts when its context is cancelled; a fake that ignores
	// the context cannot show what sharing one between callers costs.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.listErr.Load(); err != nil {
		return nil, *err
	}
	entries, ok := b.dirs[id]
	if !ok {
		return nil, pan115.ErrNotFound
	}
	return entries, nil
}

// failListings makes every subsequent listing fail, which is how an expired
// login or a dead network reaches the WebDAV layer.
func (b *fakeBackend) failListings(err error) { b.listErr.Store(&err) }

// failResolves does the same for the download endpoint, which is where an
// expiry is usually met: listings are cached for minutes, resolves are not.
func (b *fakeBackend) failResolves(err error) { b.resolveErr.Store(&err) }

func (b *fakeBackend) Resolve(ctx context.Context, pickCode string) (*pan115.Target, error) {
	b.resolveCalls.Add(1)
	if b.beforeResolve != nil {
		b.beforeResolve()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.resolveErr.Load(); err != nil {
		return nil, *err
	}
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
	if opts.Credentials == nil {
		opts.Credentials = Static{B: b}
	}
	if opts.DirTTL == 0 {
		opts.DirTTL = time.Minute
	}
	if opts.LinkTTL == 0 {
		opts.LinkTTL = time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	srv := httptest.NewServer(New(context.Background(), opts))
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

// A player hanging up mid-stream is routine: it happens after probing a
// container, on every seek, and when playback stops. It must not be reported
// as a failure. The Windows socket errors for it do not match the Unix errno
// names, which is how these came to be logged as warnings, so the check is on
// which half of the copy failed rather than on any error code.
func TestClientHangupIsNotAWarning(t *testing.T) {
	b := sample(t)
	// Large enough that the copy is still running when the client goes away.
	b.blobs["pc-film"] = []byte(strings.Repeat("payload-", 2<<20))
	b.dirs["0"][1].Size = int64(len(b.blobs["pc-film"]))

	var logs safeBuffer
	srv := newTestServer(t, b, Options{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/film.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, resp.Body, 4096); err != nil {
		t.Fatalf("reading the start of the stream: %v", err)
	}
	cancel() // the player goes away mid-file
	resp.Body.Close()

	// Give the server a moment to notice the closed connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "player closed the stream") {
		time.Sleep(10 * time.Millisecond)
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") || strings.Contains(got, "level=ERROR") {
		t.Errorf("a client hangup was logged as a failure:\n%s", got)
	}
}

// safeBuffer is a bytes.Buffer usable from the serving goroutine and the test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A failed listing must never reach the client as an empty directory.
//
// x/net/webdav writes the 207 status line before it walks, and treats a
// PathError from Readdir as "skip this directory", so without a pre-flight the
// reply is a well-formed multi-status containing nothing -- which a player
// reads as a library that has been emptied.
func TestPropfindDoesNotReportAnEmptyDirectoryOnFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantStatus int
	}{
		"credentials being refreshed": {ErrUnavailable, http.StatusServiceUnavailable},
		"backend broken":              {errors.New("connection reset"), http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			b := sample(t)
			srv := newTestServer(t, b, Options{RetryAfter: 30 * time.Second})
			b.failListings(tc.err)

			resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body:\n%s", resp.StatusCode, tc.wantStatus, body(t, resp))
			}
			if strings.Contains(body(t, resp), "<D:multistatus") {
				t.Error("answered with a multi-status; a partial listing is worse than an error")
			}
		})
	}
}

// Retry-After tells a player when to come back rather than leaving it to
// hammer the mount.
func TestUnavailableCarriesRetryAfter(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{RetryAfter: 30 * time.Second})
	b.failListings(ErrUnavailable)

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
}

// Depth: 0 asks about the directory itself, so a broken listing is beside the
// point and must not turn into an error.
func TestPropfindDepthZeroNeedsNoListing(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})
	b.failListings(ErrUnavailable)

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	if n := b.listCalls.Load(); n != 0 {
		t.Errorf("listed %d times for a Depth: 0 request, want 0", n)
	}
}

// An unbounded walk would enumerate the whole account one rate-limited request
// at a time, so it is refused the way RFC 4918 provides for.
func TestUnboundedPropfindIsRefused(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	for _, depth := range []string{"infinity", ""} {
		headers := map[string]string{"Depth": depth}
		if depth == "" {
			headers = nil // absent means infinity
		}
		resp := do(t, srv, "PROPFIND", "/", headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Depth %q = %d, want 403", depth, resp.StatusCode)
		}
		if got := body(t, resp); !strings.Contains(got, "propfind-finite-depth") {
			t.Errorf("Depth %q body does not name the precondition:\n%s", depth, got)
		}
	}
	if n := b.listCalls.Load(); n != 0 {
		t.Errorf("listed %d times while refusing, want 0", n)
	}
}

// fakeCredentials stands in for whatever holds the 115 login: it hands out
// backends in order, can refuse to supply one at all, and records rejections.
type fakeCredentials struct {
	mu       sync.Mutex
	queue    []Backend
	err      error
	handed   []Backend
	rejected []Backend
}

func (c *fakeCredentials) Backend(context.Context) (Backend, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if len(c.queue) == 0 {
		return nil, errors.New("no credentials left")
	}
	next := c.queue[0]
	if len(c.queue) > 1 {
		c.queue = c.queue[1:]
	}
	c.handed = append(c.handed, next)
	return next, nil
}

func (c *fakeCredentials) Reject(backend Backend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejected = append(c.rejected, backend)
}

func (c *fakeCredentials) counts() (handed, rejected int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.handed), len(c.rejected)
}

// A source with nothing usable to give refuses every request outright, without
// asking 115 anything -- which is the point of the blackout it is reporting.
func TestNoCredentialsRefusesEverythingWithoutAsking115(t *testing.T) {
	b := sample(t)
	creds := &fakeCredentials{queue: []Backend{b}, err: errors.New("waiting for a new login")}
	srv := newTestServer(t, b, Options{Credentials: creds, RetryAfter: 15 * time.Second})

	for _, tc := range []struct{ method, path string }{
		{"PROPFIND", "/"}, {http.MethodGet, "/film.mkv"}, {http.MethodHead, "/film.mkv"}, {http.MethodGet, "/"},
	} {
		resp := do(t, srv, tc.method, tc.path, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Errorf("%s %s carried no Retry-After", tc.method, tc.path)
		}
	}
	if n := b.listCalls.Load() + b.resolveCalls.Load(); n != 0 {
		t.Errorf("made %d backend calls with no credentials, want 0", n)
	}

	// OPTIONS still answers, so a client can tell the mount apart from a dead
	// port and knows to come back.
	if resp := do(t, srv, http.MethodOptions, "/", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("OPTIONS = %d, want 200 even while unavailable", resp.StatusCode)
	}

	creds.mu.Lock()
	creds.err = nil
	creds.mu.Unlock()
	if resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("status = %d after recovery, want 207", resp.StatusCode)
	}
}

// Replacing the credentials must replace everything derived from them. The
// channel may by then hold a different 115 account, whose directory ids and
// pick codes mean nothing here -- so serving one cached entry from before the
// swap is serving another account's file.
//
// Nothing is emptied to achieve this. The caches belong to the epoch that was
// retired, and the new epoch simply has its own.
func TestNothingCachedUnderOldCredentialsSurvivesTheSwap(t *testing.T) {
	first, second := sample(t), sample(t)
	second.blobs["pc-film"] = []byte("a completely different film")
	second.dirs["0"][1] = pan115.Entry{
		ID: "f9", Name: "film.mkv", Size: int64(len(second.blobs["pc-film"])),
		PickCode: "pc-film", SHA1: "different", ModTime: time.Unix(1700000100, 0),
	}

	creds := &fakeCredentials{queue: []Backend{first, second}}
	srv := newTestServer(t, first, Options{
		Credentials: creds, DirTTL: time.Hour, LinkTTL: time.Hour,
	})

	if got := body(t, do(t, srv, http.MethodGet, "/film.mkv", nil)); !strings.HasPrefix(got, "film-payload-") {
		t.Fatalf("first read served %.20q, want the first account's film", got)
	}
	listed, resolved := first.listCalls.Load(), first.resolveCalls.Load()

	// Warm: neither the listing nor the link is fetched again.
	do(t, srv, http.MethodGet, "/film.mkv", nil)
	if first.listCalls.Load() != listed || first.resolveCalls.Load() != resolved {
		t.Fatal("a warm cache still went to the backend")
	}

	// 115 stops accepting the login. It is discovered by the next request that
	// actually needs the backend -- a warm cache cannot notice, and does not
	// have to: it is discarded on the swap either way, which is the point.
	first.failListings(pan115.ErrNotAuthorized)
	if resp := do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("the request that met the expiry got %d, want it served from the new credentials", resp.StatusCode)
	}

	got := body(t, do(t, srv, http.MethodGet, "/film.mkv", nil))
	if got != "a completely different film" {
		t.Errorf("after the swap the mount served %q, want the second account's film", got)
	}
	if handed, rejected := creds.counts(); handed != 2 || rejected != 1 {
		t.Errorf("credentials handed out %d times and rejected %d, want 2 and 1", handed, rejected)
	}
	if second.resolveCalls.Load() == 0 {
		t.Error("the link cache from the old credentials was reused after the swap")
	}
}

// Retiring an epoch cancels the shared work started under it. Without that,
// a listing already in flight would finish against credentials that have been
// abandoned and store its result -- and if the caches outlived the swap, that
// result would then be served.
func TestRetiringAnEpochCancelsItsSharedWork(t *testing.T) {
	b := sample(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b.beforeList = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	creds := &fakeCredentials{queue: []Backend{b}}
	server := New(context.Background(), Options{
		Credentials: creds, DirTTL: time.Hour, LinkTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)

	done := make(chan int, 1)
	go func() {
		resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
		done <- resp.StatusCode
	}()
	<-entered

	// The credentials this listing was started under are retired underneath it.
	server.epochs.close()
	close(release)

	select {
	case status := <-done:
		if status == http.StatusMultiStatus {
			t.Error("a listing survived the retirement of the credentials it was made with")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never finished after its epoch was retired")
	}
}

// A file must look like the same file however it was asked about. The CDN
// answers with its own ETag and its own modification time; if those reached
// the client it would see one identity through PROPFIND and another through
// GET, and could not tell whether its cached copy was still good.
func TestFileIdentityIsConsistentAcrossMethods(t *testing.T) {
	srv := newTestServer(t, sample(t), Options{})

	head := do(t, srv, http.MethodHead, "/film.mkv", nil)
	get := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{"Range": "bytes=0-9"})
	propfind := body(t, do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}))

	wantTag := `"sha1:abc123"`
	for name, resp := range map[string]*http.Response{"HEAD": head, "GET": get} {
		if got := resp.Header.Get("ETag"); got != wantTag {
			t.Errorf("%s ETag = %q, want %q", name, got, wantTag)
		}
		if got := resp.Header.Get("Last-Modified"); got != head.Header.Get("Last-Modified") {
			t.Errorf("%s Last-Modified = %q, want %q", name, got, head.Header.Get("Last-Modified"))
		}
		if got := resp.Header.Get("Content-Type"); got != "video/x-matroska" {
			t.Errorf("%s Content-Type = %q, want video/x-matroska", name, got)
		}
	}
	if !strings.Contains(propfind, "sha1:abc123") {
		t.Errorf("PROPFIND reports a different identity:\n%s", propfind)
	}
}

// If-Range names a validator only this server has issued, so it is answered
// here: forwarded upstream it would mean nothing to the CDN, which would
// answer with the whole file just as the client seeked.
func TestIfRangeIsAnsweredLocally(t *testing.T) {
	for name, tc := range map[string]struct {
		ifRange    string
		wantStatus int
	}{
		"matching etag": {`"sha1:abc123"`, http.StatusPartialContent},
		"weak etag":     {`W/"sha1:abc123"`, http.StatusPartialContent},
		"stale etag":    {`"sha1:something-else"`, http.StatusOK},
		"stale date":    {"Mon, 02 Jan 2006 15:04:05 GMT", http.StatusOK},
		"absent":        {"", http.StatusPartialContent},
	} {
		t.Run(name, func(t *testing.T) {
			b := sample(t)
			srv := newTestServer(t, b, Options{})
			headers := map[string]string{"Range": "bytes=0-99"}
			if tc.ifRange != "" {
				headers["If-Range"] = tc.ifRange
			}

			resp := do(t, srv, http.MethodGet, "/film.mkv", headers)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := len(body(t, resp)); tc.wantStatus == http.StatusOK && got != len(b.blobs["pc-film"]) {
				t.Errorf("served %d bytes, want the whole file (%d)", got, len(b.blobs["pc-film"]))
			}
		})
	}
}

// A connection that dies before the CDN answers is worth one more try: nothing
// has reached the client yet, and a dropped connection is not a dead link.
func TestUpstreamConnectionFailureIsRetried(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})
	b.dropOnce.Store(true)

	resp := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{"Range": "bytes=0-99"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 after one retry", resp.StatusCode)
	}
	if got := len(body(t, resp)); got != 100 {
		t.Errorf("served %d bytes, want 100", got)
	}
	if n := b.fetches.Load(); n != 2 {
		t.Errorf("fetched %d times, want 2 -- one failure and one retry", n)
	}
	if n := b.resolveCalls.Load(); n != 1 {
		t.Errorf("resolved %d times, want 1 -- the link was fine, the connection was not", n)
	}
}

// Two failures in a row is upstream being genuinely broken, and the client is
// told so rather than being made to wait through further attempts.
func TestUpstreamFailureIsReportedAfterOneRetry(t *testing.T) {
	b := newFakeBackend(t)
	b.blobs["pc-gone"] = []byte("x")
	b.dirs["0"] = []pan115.Entry{{ID: "f1", Name: "gone.mkv", Size: 1, PickCode: "pc-gone"}}
	srv := newTestServer(t, b, Options{})
	b.origin.Close() // upstream is down for good

	resp := do(t, srv, http.MethodGet, "/gone.mkv", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// The login usually expires while the directory listing is still cached, so
// the first thing to notice is the resolve -- not the lookup. A retry that
// covered only the lookup would almost never fire, and the player would get a
// 502 for a mount that could have healed itself.
//
// Nothing has been written to the client at that point, which is what makes
// starting again with new credentials possible at all.
func TestAnExpiryFoundAtResolveTimeStillRecovers(t *testing.T) {
	first, second := sample(t), sample(t)
	second.blobs["pc-film"] = []byte("served by the replacement login")
	second.dirs["0"][1] = pan115.Entry{
		ID: "f1", Name: "film.mkv", Size: int64(len(second.blobs["pc-film"])),
		PickCode: "pc-film", SHA1: "abc123", ModTime: time.Unix(1700000100, 0),
	}

	creds := &fakeCredentials{queue: []Backend{first, second}}
	srv := newTestServer(t, first, Options{Credentials: creds, DirTTL: time.Hour, LinkTTL: time.Hour})

	// Warm the listing, so the lookup below cannot be what notices.
	if resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("warm-up = %d, want 207", resp.StatusCode)
	}
	listed := first.listCalls.Load()

	first.failResolves(pan115.ErrNotAuthorized)

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- the mount did not recover from an expiry met at resolve time", resp.StatusCode)
	}
	if got := body(t, resp); got != "served by the replacement login" {
		t.Errorf("served %q, want the replacement account's film", got)
	}
	if first.listCalls.Load() != listed {
		t.Error("the warm listing was refetched; the lookup was not the thing that noticed")
	}
	if handed, rejected := creds.counts(); handed != 2 || rejected != 1 {
		t.Errorf("credentials handed out %d times and rejected %d, want 2 and 1", handed, rejected)
	}
}
