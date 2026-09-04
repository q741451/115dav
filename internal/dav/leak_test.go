package dav

import (
	"context"
	"fmt"
	"io"
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
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond, LinkTTL: 50 * time.Millisecond})

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
	}

	settle(t)
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

	tree := NewTree(b, "0", time.Hour)
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
