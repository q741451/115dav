package dav

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

func testSlots(limit int) *slots {
	s := newSlots(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.limit = limit
	return s
}

// Up to the limit, everyone is admitted and nobody is disturbed.
func TestStreamsUpToTheLimitCoexist(t *testing.T) {
	s := testSlots(2)
	ctx := context.Background()

	first, a, releaseA := s.acquire(ctx, "pc", "film.mkv")
	second, bSlot, releaseB := s.acquire(ctx, "pc", "film.mkv")
	defer releaseA()
	defer releaseB()

	if first.Err() != nil || second.Err() != nil {
		t.Error("a stream was cut although the file was under its limit")
	}
	if s.evicted(a) || s.evicted(bSlot) {
		t.Error("a slot reports itself evicted while it is still held")
	}
}

// The newest request wins, and the oldest is the one that goes.
func TestANewStreamEvictsTheOldest(t *testing.T) {
	s := testSlots(2)
	ctx := context.Background()

	oldest, first, _ := s.acquire(ctx, "pc", "film.mkv")
	middle, second, _ := s.acquire(ctx, "pc", "film.mkv")
	newest, third, release := s.acquire(ctx, "pc", "film.mkv")
	defer release()

	if oldest.Err() == nil {
		t.Error("the oldest stream was not evicted to make room")
	}
	if !s.evicted(first) {
		t.Error("the evicted slot does not know it")
	}
	if middle.Err() != nil || newest.Err() != nil {
		t.Error("a stream that should have been kept was cut")
	}
	if s.evicted(second) || s.evicted(third) {
		t.Error("a held slot reports itself evicted")
	}
}

// The limit is per file. Two people watching two different films is the case
// this exists to keep working.
func TestDifferentFilesDoNotEvictEachOther(t *testing.T) {
	s := testSlots(2)
	ctx := context.Background()

	one, _, releaseOne := s.acquire(ctx, "pc-a", "a.mkv")
	two, _, releaseTwo := s.acquire(ctx, "pc-b", "b.mkv")
	three, _, releaseThree := s.acquire(ctx, "pc-c", "c.mkv")
	defer releaseOne()
	defer releaseTwo()
	defer releaseThree()

	for i, ctx := range []context.Context{one, two, three} {
		if ctx.Err() != nil {
			t.Errorf("stream %d of a different file was evicted", i)
		}
	}
}

// Releasing frees the slot for the next arrival rather than leaving it counted
// -- a stream that ends normally must not make the file look busy.
func TestReleasingFreesTheSlot(t *testing.T) {
	s := testSlots(1)
	ctx := context.Background()

	_, _, release := s.acquire(ctx, "pc", "film.mkv")
	release()

	next, _, releaseNext := s.acquire(ctx, "pc", "film.mkv")
	defer releaseNext()
	if next.Err() != nil {
		t.Error("the next stream was evicted by one that had already finished")
	}

	s.mu.Lock()
	held := len(s.held)
	s.mu.Unlock()
	if held != 1 {
		t.Errorf("the map holds %d files, want 1", held)
	}
}

// Nothing accumulates: the map is keyed by pick code, and a server that has
// streamed ten thousand files must not still be holding ten thousand keys.
func TestSlotsDoNotAccumulate(t *testing.T) {
	s := testSlots(2)
	ctx := context.Background()

	for i := range 1000 {
		code := "pc" + strings.Repeat("x", i%7)
		_, _, release := s.acquire(ctx, code, "film.mkv")
		release()
	}

	s.mu.Lock()
	held := len(s.held)
	s.mu.Unlock()
	if held != 0 {
		t.Errorf("%d files still hold slots after every stream ended", held)
	}
}

// Release is called on a slot that was already evicted, every time a cut
// stream unwinds. It must not disturb whoever took its place.
func TestReleasingAnEvictedSlotIsHarmless(t *testing.T) {
	s := testSlots(1)
	ctx := context.Background()

	_, first, releaseFirst := s.acquire(ctx, "pc", "film.mkv")
	kept, second, releaseSecond := s.acquire(ctx, "pc", "film.mkv")
	defer releaseSecond()

	if !s.evicted(first) {
		t.Fatal("the first slot was not evicted")
	}
	releaseFirst() // as the cut stream unwinds
	releaseFirst() // and again, since release is deferred and may double up

	if kept.Err() != nil {
		t.Error("releasing an evicted slot cut the stream that replaced it")
	}
	if s.evicted(second) {
		t.Error("the surviving slot was removed by someone else's release")
	}
}

func TestSlotsAreSafeUnderConcurrency(t *testing.T) {
	s := testSlots(2)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := "pc" + string(rune('a'+i%5))
			_, _, release := s.acquire(ctx, code, "film.mkv")
			time.Sleep(time.Millisecond)
			release()
		}()
	}
	wg.Wait()

	s.mu.Lock()
	held := len(s.held)
	s.mu.Unlock()
	if held != 0 {
		t.Errorf("%d files still hold slots", held)
	}
}

// End to end: a third reader of one file cuts the first, and the first's
// client sees the stream stop rather than the third being refused.
func TestAThirdReaderOfOneFileCutsTheFirst(t *testing.T) {
	b := sample(t)
	// A body big enough that the first two are still mid-transfer when the
	// third arrives.
	b.blobs["pc-film"] = []byte(strings.Repeat("x", 4<<20))
	b.dirs["0"][1] = pan115.Entry{
		ID: "f1", Name: "film.mkv", Size: int64(len(b.blobs["pc-film"])),
		PickCode: "pc-film", SHA1: "abc123", ModTime: time.Unix(1700000100, 0),
	}
	srv := newTestServer(t, b, Options{})

	// Two slow readers, holding their slots.
	started := make(chan struct{}, 2)
	stopped := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := srv.Client().Get(srv.URL + "/film.mkv")
			if err != nil {
				started <- struct{}{}
				stopped <- -1
				return
			}
			defer resp.Body.Close()
			io.CopyN(io.Discard, resp.Body, 1024)
			started <- struct{}{}
			// Read the rest slowly enough that the third request arrives first.
			n, _ := io.Copy(io.Discard, slowReader{resp.Body})
			stopped <- int(n)
		}()
	}
	<-started
	<-started

	// The third arrives and must be served, not refused.
	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the third reader got %d, want 200 -- it should have evicted, not been refused", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)

	// And at least one of the first two was cut short.
	var short bool
	for range 2 {
		if n := <-stopped; n >= 0 && n < len(b.blobs["pc-film"])-1024 {
			short = true
		}
	}
	if !short {
		t.Error("no earlier stream was cut, so the file was served three times at once")
	}
}

// slowReader paces a copy so that a test can be sure a later request overtakes
// an earlier one.
type slowReader struct{ r io.Reader }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(2 * time.Millisecond)
	if len(p) > 4096 {
		p = p[:4096]
	}
	return s.r.Read(p)
}

// The window the eviction leaves open: cancelling our request is instant, but
// 115 stops counting it only when the teardown reaches it, and this side
// cannot see when that is. So the refusal is the signal -- wait a moment and
// ask again, keeping the link, rather than guessing at a delay beforehand.
func TestAThrottledReadWaitsAndSucceeds(t *testing.T) {
	b := sample(t)
	b.throttleFirst.Store(2) // busy for the first two attempts, then not
	srv := newTestServer(t, b, Options{})

	resolves := b.resolveCalls.Load()
	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the file was free", resp.StatusCode)
	}
	if n := len(body(t, resp)); n != len(b.blobs["pc-film"]) {
		t.Errorf("served %d bytes, want %d", n, len(b.blobs["pc-film"]))
	}
	// The link was never in doubt, so it must not have been fetched again --
	// that endpoint allows two calls a second and is shared with every listing.
	if got := b.resolveCalls.Load() - resolves; got != 1 {
		t.Errorf("resolved %d times for a file that was merely busy, want 1", got)
	}
}

// A file that stays busy is a 503 with a Retry-After, not the 502 that says
// something is wrong. It is temporary, and the player should come back.
func TestAPersistentlyThrottledReadIs503(t *testing.T) {
	b := sample(t)
	b.throttleFirst.Store(1 << 20)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on a refusal that is worth retrying")
	}
	if got := b.resolveCalls.Load(); got != 1 {
		t.Errorf("resolved %d times while being throttled, want 1", got)
	}
}

// A 403 that is not a throttle still means the link is finished, and the only
// way forward is to resolve again. The two must not be confused: treating a
// throttle as expiry spends a rate-limited call for nothing, and treating
// expiry as a throttle waits for a URL that will never work.
func TestAnExpiredLinkIsStillResolvedAgain(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	if resp := do(t, srv, http.MethodGet, "/film.mkv", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first read = %d", resp.StatusCode)
	}
	resolves := b.resolveCalls.Load()

	b.generation.Add(1) // every URL handed out so far is now refused

	if resp := do(t, srv, http.MethodGet, "/film.mkv", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after resolving again", resp.StatusCode)
	}
	if got := b.resolveCalls.Load() - resolves; got != 1 {
		t.Errorf("resolved %d times for an expired link, want 1", got)
	}
}

// A read cut to admit a newer one is not a broken gateway. It is this moment
// being busy, it passes in seconds, and the player should be told to come back
// rather than that something is wrong with the file.
func TestAnEvictedReadIs503NotABadGateway(t *testing.T) {
	b := sample(t)
	b.blobs["pc-film"] = []byte(strings.Repeat("x", 4<<20))
	b.dirs["0"][1] = pan115.Entry{
		ID: "f1", Name: "film.mkv", Size: int64(len(b.blobs["pc-film"])),
		PickCode: "pc-film", SHA1: "abc123", ModTime: time.Unix(1700000100, 0),
	}

	// Hold the CDN so every read is still opening when the next arrives, which
	// is how a request gets evicted before it has written anything.
	release := make(chan struct{})
	var once sync.Once
	b.beforeResolve = func() { <-release }
	defer once.Do(func() { close(release) })

	srv := newTestServer(t, b, Options{})
	srv.Config.Handler.(*Server).stream.slots.limit = 1

	codes := make(chan int, 3)
	for range 3 {
		go func() {
			resp, err := srv.Client().Get(srv.URL + "/film.mkv")
			if err != nil {
				codes <- 0
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			codes <- resp.StatusCode
		}()
		time.Sleep(30 * time.Millisecond) // establish an order
	}
	once.Do(func() { close(release) })

	var got []int
	for range 3 {
		got = append(got, <-codes)
	}
	for _, code := range got {
		if code == http.StatusBadGateway {
			t.Errorf("an evicted read was answered %d; want 200 or 503, never a bad gateway", code)
		}
	}
	t.Logf("statuses: %v", got)
}

// A read can be evicted while it is waiting out a throttle, not only while it
// is opening. Both are the same event to the client -- it asked for a file and
// somebody newer took the slot -- so both must answer the same way. The two
// exits were not always in step: one reported it, the other returned a bare
// cancellation and became a bad gateway.
func TestAReadEvictedWhileWaitingOutAThrottleIs503(t *testing.T) {
	b := sample(t)
	b.throttleFirst.Store(1 << 20) // permanently busy, so every read waits
	srv := newTestServer(t, b, Options{})
	srv.Config.Handler.(*Server).stream.slots.limit = 1

	codes := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := srv.Client().Get(srv.URL + "/film.mkv")
			if err != nil {
				codes <- 0
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			codes <- resp.StatusCode
		}()
		// Long enough that the first is inside its backoff when the second
		// arrives and takes the slot from it.
		time.Sleep(100 * time.Millisecond)
	}

	var got []int
	for range 2 {
		got = append(got, <-codes)
	}
	for _, code := range got {
		if code != http.StatusServiceUnavailable {
			t.Errorf("got %d; a read that lost its slot mid-wait is 503, never %d", code, code)
		}
	}
	t.Logf("statuses: %v", got)
}
