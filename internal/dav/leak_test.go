package dav

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// A player cancelling one request must not fail the others waiting on the
// same work. singleflight shares whichever context arrived first, so without
// care the first client to hang up takes the rest down with it -- and hanging
// up is what a media client does constantly, on every seek and every time a
// user backs out of a folder.
func TestCancelledRequestDoesNotPoisonSharedWork(t *testing.T) {
	for name, tc := range map[string]struct {
		hook   func(*fakeBackend, func())
		method string
		header map[string]string
		want   int
		calls  func(*fakeBackend) int64
	}{
		"listing": {
			hook:   func(b *fakeBackend, block func()) { b.beforeList = block },
			method: "PROPFIND",
			header: map[string]string{"Depth": "1"},
			want:   http.StatusMultiStatus,
			calls:  func(b *fakeBackend) int64 { return b.listCalls.Load() },
		},
		"resolve": {
			hook:   func(b *fakeBackend, block func()) { b.beforeResolve = block },
			method: http.MethodGet,
			header: map[string]string{"Range": "bytes=0-99"},
			want:   http.StatusPartialContent,
			calls:  func(b *fakeBackend) int64 { return b.resolveCalls.Load() },
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := sample(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			tc.hook(b, func() {
				once.Do(func() { close(entered) })
				<-release
			})
			srv := newTestServer(t, b, Options{})
			// This test is about several requests sharing one piece of work,
			// so the file limit -- which would cut all but the newest two of
			// them -- has to be out of the way. slots is covered separately.
			unlimitStreams(t, srv)
			path := "/"
			if tc.method == http.MethodGet {
				path = "/film.mkv"
			}

			// The doomed caller gets there first and becomes the one doing the
			// work on everyone's behalf.
			doomed, cancel := context.WithCancel(context.Background())
			go func() {
				req, _ := http.NewRequestWithContext(doomed, tc.method, srv.URL+path, nil)
				for k, v := range tc.header {
					req.Header.Set(k, v)
				}
				if resp, err := srv.Client().Do(req); err == nil {
					resp.Body.Close()
				}
			}()
			<-entered

			var wg sync.WaitGroup
			statuses := make(chan int, 4)
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, _ := http.NewRequest(tc.method, srv.URL+path, nil)
					for k, v := range tc.header {
						req.Header.Set(k, v)
					}
					resp, err := srv.Client().Do(req)
					if err != nil {
						statuses <- 0
						return
					}
					defer resp.Body.Close()
					io.Copy(io.Discard, resp.Body)
					statuses <- resp.StatusCode
				}()
			}

			time.Sleep(150 * time.Millisecond) // let them queue behind the leader
			cancel()                           // the player hangs up
			time.Sleep(50 * time.Millisecond)
			close(release) // 115 finally answers

			wg.Wait()
			close(statuses)
			for status := range statuses {
				if status != tc.want {
					t.Errorf("a waiting request got %d after another client hung up, want %d", status, tc.want)
				}
			}
			if n := tc.calls(b); n != 1 {
				t.Errorf("backend was called %d times, want 1 -- the requests did not share the work", n)
			}
		})
	}
}

// Churn the server the way weeks of use would, and check that nothing
// accumulates: no goroutine left behind, no descriptor, no growing cache.
func TestNothingAccumulatesUnderChurn(t *testing.T) {
	b := sample(t)
	// Two accounts, handed out alternately, so the run exercises credentials
	// being replaced as well as requests being served. A rotation builds a
	// whole new epoch -- caches, contexts and all -- and weeks of unattended
	// use is mostly this.
	// Built up front, not in the loop: each fake backend runs an httptest
	// origin whose goroutines would otherwise show up as growth that is the
	// test's own doing.
	fakes := []*fakeBackend{b}
	for range 12 {
		fakes = append(fakes, newFakeBackend(t))
	}
	rotations := make([]Backend, len(fakes))
	for i, f := range fakes {
		rotations[i] = f
	}
	creds := &fakeCredentials{queue: rotations}

	// Every backend has to refuse at once. Failing only the first would stop
	// working after the first rotation, when it is no longer the one in use --
	// and the run would go quiet rather than fail.
	refuseEverywhere := func(err error) {
		for _, f := range fakes {
			f.listErr.Store(&err)
		}
	}
	acceptEverywhere := func() {
		for _, f := range fakes {
			f.listErr.Store(nil)
		}
	}
	srv := newTestServer(t, b, Options{
		Credentials: creds,
		DirTTL:      50 * time.Millisecond,
		LinkTTL:     50 * time.Millisecond,
	})

	settle(t)
	goroutines, fds := runtime.NumGoroutine(), openFiles(t)

	for round := range 60 {
		// A whole listing, read to completion.
		resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// A range request abandoned part way, as on every seek.
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/film.mkv", nil)
		if resp, err := srv.Client().Do(req); err == nil {
			io.CopyN(io.Discard, resp.Body, 512)
			resp.Body.Close()
		}
		cancel()

		// A link that has gone stale upstream, forcing a re-resolve.
		if round%10 == 0 {
			b.generation.Add(1)
		}
		// A file that is not there.
		do(t, srv, http.MethodGet, "/missing.mkv", nil).Body.Close()

		// The login expires and is replaced, which retires an epoch and builds
		// another. Everything the old one held has to go with it.
		if round%7 == 0 {
			// Past the TTL, so the listing is actually fetched rather than
			// served from the cache the PROPFIND above just warmed. Without
			// this the expiry is never met and nothing rotates.
			time.Sleep(60 * time.Millisecond)
			refuseEverywhere(pan115.ErrNotAuthorized)
			do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}).Body.Close()
			acceptEverywhere()
		}
	}

	settle(t)
	// The run has to have done what it claims, or the counts below measure an
	// idle server. A vacuous soak is worse than none: it reports safety.
	handed, rejected := creds.counts()
	t.Logf("rotations: handed=%d rejected=%d", handed, rejected)
	if handed < 5 || rejected < 5 {
		t.Fatalf("credentials were handed out %d times and rejected %d; the run never rotated", handed, rejected)
	}
	if grew := runtime.NumGoroutine() - goroutines; grew > 4 {
		t.Errorf("goroutines grew by %d over the run (from %d)", grew, goroutines)
		buf := make([]byte, 1<<16)
		t.Logf("stacks:\n%s", buf[:runtime.Stack(buf, true)])
	}
	if grew := openFiles(t) - fds; grew > 4 {
		t.Errorf("open descriptors grew by %d over the run (from %d)", grew, fds)
	}
}

// settle waits for in-flight request goroutines to finish, since a client
// closing a connection is noticed asynchronously.
func settle(t *testing.T) {
	t.Helper()
	for range 50 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

// openFiles counts descriptors held by this process. It only works on Linux,
// which is where this runs in CI and on the router it is built for.
func openFiles(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		return 0
	}
	names, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count descriptors: %v", err)
	}
	return len(names)
}

// A directory cache bounded by directory count says nothing about memory: it
// is the entries that take the space, and a library scan walks thousands of
// directories in minutes.
func TestDirectoryCacheIsBoundedByEntries(t *testing.T) {
	b := newFakeBackend(t)
	const perDir = 500
	for d := range 200 {
		id := fmt.Sprint("d", d)
		entries := make([]pan115.Entry, perDir)
		for i := range entries {
			entries[i] = pan115.Entry{ID: fmt.Sprint(id, "-", i), Name: fmt.Sprint("file", i, ".mkv"), Size: 1}
		}
		b.dirs[id] = entries
	}

	tree := NewTree(context.Background(), b, "0", time.Hour)
	for d := range 200 {
		if _, err := tree.Children(context.Background(), fmt.Sprint("d", d)); err != nil {
			t.Fatal(err)
		}
	}

	tree.mu.Lock()
	dirs, entries := len(tree.dirs), 0
	for _, dir := range tree.dirs {
		entries += len(dir.entries)
	}
	tree.mu.Unlock()

	if entries > maxCachedEntries {
		t.Errorf("cache holds %d entries across %d directories, over the %d cap",
			entries, dirs, maxCachedEntries)
	}
}

// heapAfterGC reports live heap bytes, with the garbage actually collected.
// Two cycles: the first frees, the second measures what the first could not.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Retiring an epoch has to release everything it held, and a context is the
// part that will not release itself.
//
// Each epoch derives a context from the server's, and a derived context stays
// registered with its parent until it is cancelled -- so an epoch that is
// dropped without being cancelled leaves a node on a list that lives as long
// as the process. Nothing would look wrong: the caches go, the client goes,
// and a router picking up a new cookie every few hours quietly accumulates one
// of these for each until it runs out of memory.
//
// The rotation count is far past anything real. It is chosen so that a leak of
// a couple of hundred bytes apiece is well clear of the noise in a heap
// measurement, which is the only signal available from outside the context
// package.
func TestRetiringAnEpochReleasesIt(t *testing.T) {
	b := sample(t)
	owner, stop := context.WithCancel(context.Background())
	defer stop()

	// A source that hands out a different client every time, which is what
	// makes rotate actually rotate. What is behind it does not matter; the
	// epoch lifecycle is what is under test.
	creds := &alwaysNew{}
	e := &epochs{
		owner: owner,
		creds: creds,
		opts:  Options{DirTTL: time.Minute, LinkTTL: time.Minute},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_ = b

	churn := func() {
		current, err := e.get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := e.rotate(context.Background(), current)
		if err != nil {
			t.Fatal(err)
		}
		// Without this the loop can spin without rotating anything and the
		// measurement below means nothing. It caught exactly that: an inert
		// backend that was a zero-size struct, every instance of which Go
		// allocates at the same address, so rotate decided the source had
		// handed back what it already had.
		if fresh == nil || fresh == current {
			t.Fatal("rotate did not replace the epoch; the test would measure nothing")
		}
	}

	// Warm up, so the measurement is not dominated by first-use allocation.
	for range 100 {
		churn()
	}

	settle(t)
	before, goroutines, fds := heapAfterGC(), runtime.NumGoroutine(), openFiles(t)

	const rotations = 20000
	for range rotations {
		churn()
	}

	settle(t)
	after := heapAfterGC()

	// A retained context and its parent's bookkeeping run to a couple of
	// hundred bytes; 20000 of them is megabytes. The allowance is far above
	// the noise and far below a leak.
	const allowance = 1 << 20
	if after > before+allowance {
		t.Errorf("heap grew by %d bytes over %d rotations (from %d to %d); a retired epoch is still referenced",
			after-before, rotations, before, after)
	}
	if grew := runtime.NumGoroutine() - goroutines; grew > 4 {
		t.Errorf("goroutines grew by %d over %d rotations", grew, rotations)
	}
	if grew := openFiles(t) - fds; grew > 4 {
		t.Errorf("open descriptors grew by %d over %d rotations", grew, rotations)
	}
}

// alwaysNew is a credential source with an inexhaustible supply of distinct
// clients, so that every rotation really replaces one.
type alwaysNew struct{}

func (*alwaysNew) Backend(context.Context) (Backend, error) { return &inertBackend{}, nil }
func (*alwaysNew) Reject(Backend)                           {}

// inertBackend carries a field so that instances have distinct addresses. A
// zero-size struct would not: Go allocates them all at one address, and every
// pointer to one compares equal to every other.
type inertBackend struct{ _ byte }

func (*inertBackend) List(context.Context, string) ([]pan115.Entry, error) { return nil, nil }
func (*inertBackend) Resolve(context.Context, string) (*pan115.Target, error) {
	return nil, pan115.ErrNotFound
}
