package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/cookiesync"
	"github.com/q741451/115dav/internal/dav"
	"github.com/q741451/115dav/internal/pan115"
)

// source hands out whatever cookie is currently "on the server", and counts
// how often it is asked.
type source struct {
	mu      sync.Mutex
	cookie  string
	err     error
	fetches atomic.Int64
	delay   time.Duration
}

func (s *source) Fetch(context.Context) (string, error) {
	s.fetches.Add(1)
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie, s.err
}

func (s *source) set(cookie string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookie, s.err = cookie, err
}

// cookie builds a cookie that pan115 will accept, tagged so that different
// logins can be told apart.
func cookie(tag string) string {
	return "UID=" + tag + "; CID=c; SEID=s; KID=k"
}

// harness wires a Session to a fake 115 whose acceptance can be changed.
type harness struct {
	*Session
	src      *source
	accepted atomic.Value // string: the cookie 115 currently accepts
	calls    atomic.Int64
	flushes  atomic.Int64
}

func newHarness(t *testing.T, startCookie, accepts string) *harness {
	t.Helper()
	h := &harness{src: &source{cookie: startCookie}}
	h.accepted.Store(accepts)

	// pan115.New only parses the cookie, so a real client can stand in for a
	// fake one; the call itself is intercepted below.
	s, err := New(Options{
		Source:   h.src,
		Cookie:   startCookie,
		Blackout: 50 * time.Millisecond,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Build: func(cookie string) (*pan115.Client, error) {
			return pan115.New(pan115.Config{Cookie: cookie})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.OnRefresh(func() { h.flushes.Add(1) })
	h.Session = s
	return h
}

// call runs one operation through the refresh machinery, standing in for
// List or Resolve.
func (h *harness) call(ctx context.Context) (string, error) {
	return withRefresh(h.Session, ctx, func(c *pan115.Client) (string, error) {
		h.calls.Add(1)
		if h.currentCookie() != h.accepted.Load().(string) {
			return "", fmt.Errorf("%w: 登录超时", pan115.ErrNotAuthorized)
		}
		return "ok", nil
	})
}

func TestServesWhileCookiesAreGood(t *testing.T) {
	h := newHarness(t, cookie("good"), cookie("good"))

	for range 3 {
		if got, err := h.call(context.Background()); err != nil || got != "ok" {
			t.Fatalf("call = %q, %v; want ok", got, err)
		}
	}
	if n := h.src.fetches.Load(); n != 0 {
		t.Errorf("fetched %d times while nothing was wrong, want 0", n)
	}
}

// The point of the whole package: the login expires, someone uploads a new
// one, and the next request works without a restart.
func TestRefreshesWhenRejected(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new")) // 115 has already moved on
	h.src.set(cookie("new"), nil)                    // and the channel already has the new login

	got, err := h.call(context.Background())
	if err != nil || got != "ok" {
		t.Fatalf("call = %q, %v; want ok", got, err)
	}
	if n := h.src.fetches.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
	if n := h.flushes.Load(); n != 1 {
		t.Errorf("flushed %d times, want 1 -- caches from the old identity must go", n)
	}
	if h.Blocked() {
		t.Error("blocked after a successful refresh")
	}
}

// If the channel still holds the cookies 115 just refused, there is nothing to
// gain by trying them.
func TestDoesNotRetry115WithUnchangedCookies(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new"))
	h.src.set(cookie("old"), nil) // nobody has uploaded anything yet

	if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if n := h.calls.Load(); n != 1 {
		t.Errorf("called 115 %d times, want 1 -- the second attempt was pointless", n)
	}
	if !h.Blocked() {
		t.Error("not blocked after finding nothing new")
	}
}

// While blocked, neither 115 nor the cookie server is contacted.
func TestBlackoutTouchesNothing(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new"))
	h.src.set(cookie("old"), nil)

	if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
		t.Fatal(err)
	}
	calls, fetches := h.calls.Load(), h.src.fetches.Load()

	for range 5 {
		if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable during the blackout", err)
		}
	}
	if got := h.calls.Load(); got != calls {
		t.Errorf("called 115 %d more times during the blackout", got-calls)
	}
	if got := h.src.fetches.Load(); got != fetches {
		t.Errorf("fetched %d more times during the blackout", got-fetches)
	}
}

// Once the blackout lapses, a new upload is picked up.
func TestRecoversAfterBlackout(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new"))
	h.src.set(cookie("old"), nil)

	if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
		t.Fatal(err)
	}
	h.src.set(cookie("new"), nil) // the browser login finally happened
	time.Sleep(60 * time.Millisecond)

	if got, err := h.call(context.Background()); err != nil || got != "ok" {
		t.Fatalf("call = %q, %v; want ok once the blackout lapsed", got, err)
	}
}

// A wrong channel name or key is permanent, but must not stop the process:
// it is meant to outlive such mistakes on an unattended box.
func TestRejectedChannelBlocksButDoesNotFail(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new"))
	h.src.set("", cookiesync.ErrRejected)

	if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !h.Blocked() {
		t.Error("not blocked after the server refused the channel")
	}
}

// An incomplete cookie set -- the extension missing KID, say -- is the same
// situation as an expired one: fixed by the next upload, not by giving up.
func TestUnusableCookiesAreTreatedAsStale(t *testing.T) {
	h := newHarness(t, cookie("current"), cookie("unreachable"))
	h.src.set("UID=a; CID=b", nil) // no SEID, no KID: pan115.New will refuse

	if _, err := h.call(context.Background()); !errors.Is(err, dav.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !h.Blocked() {
		t.Error("not blocked after receiving an unusable cookie")
	}
}

// A player seeking fires many requests at once; they must not each fetch.
func TestConcurrentRejectionsFetchOnce(t *testing.T) {
	h := newHarness(t, cookie("old"), cookie("new"))
	h.src.set(cookie("new"), nil)
	h.src.delay = 20 * time.Millisecond

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.call(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := h.src.fetches.Load(); n != 1 {
		t.Errorf("fetched %d times for one expiry, want 1", n)
	}
}

// Starting with no cookie at all -- the startup fetch failed -- must still
// work once the server answers.
func TestStartsWithoutACookie(t *testing.T) {
	h := &harness{src: &source{cookie: cookie("fresh")}}
	h.accepted.Store(cookie("fresh"))
	s, err := New(Options{
		Source:   h.src,
		Blackout: 50 * time.Millisecond,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Build:    func(c string) (*pan115.Client, error) { return pan115.New(pan115.Config{Cookie: c}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	s.OnRefresh(func() { h.flushes.Add(1) })
	h.Session = s

	if got, err := h.call(context.Background()); err != nil || got != "ok" {
		t.Fatalf("call = %q, %v; want ok", got, err)
	}
	if n := h.src.fetches.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}
