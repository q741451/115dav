package dav

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// The point of the whole thing: seeking does not pay for listings.
//
// Every request walks the path from the root, so insisting the walk be current
// charged each seek up to one listing per directory in the path -- at two
// requests a second, seconds of silence before the first byte, every time the
// TTL lapsed. A walk only translates names into ids, and that answer does not
// go stale the way a listing's contents do.
func TestSeekingPastTheTTLCostsNoListing(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond, LinkTTL: time.Hour})

	if resp := do(t, srv, http.MethodGet, "/Movies/nested.mp4", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up = %d", resp.StatusCode)
	}
	listed := b.listCalls.Load()

	time.Sleep(80 * time.Millisecond) // every listing in the path is now expired

	for range 5 {
		resp := do(t, srv, http.MethodGet, "/Movies/nested.mp4", map[string]string{"Range": "bytes=0-3"})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("seek = %d, want 206", resp.StatusCode)
		}
	}
	if n := b.listCalls.Load() - listed; n != 0 {
		t.Errorf("five seeks past the TTL cost %d listings, want 0", n)
	}
}

// What the TTL is actually for: a file uploaded from the desktop shows up on
// the phone. That is a PROPFIND, and it must still refresh -- but only the
// directory being shown, not the path leading to it.
func TestBrowsingStillRefreshes(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond})

	do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"})
	listed := b.listCalls.Load()

	time.Sleep(80 * time.Millisecond)
	if resp := do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}

	// One: /Movies itself. The root was walked through, not shown.
	if n := b.listCalls.Load() - listed; n != 1 {
		t.Errorf("re-listed %d directories, want 1 -- the one being shown", n)
	}
}

// A path missing from an expired listing may simply have appeared since, so
// the listing that should have contained it is refreshed and the walk retried.
func TestAFileAddedSinceTheCachedListingIsFound(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond})
	do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})

	b.blobs["pc-new"] = []byte("uploaded just now")
	b.dirs["0"] = append(b.dirs["0"], pan115.Entry{
		ID: "f9", Name: "new.mkv", Size: 17, PickCode: "pc-new", SHA1: "new",
	})
	time.Sleep(80 * time.Millisecond)

	resp := do(t, srv, http.MethodGet, "/new.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a file added since the cached listing", resp.StatusCode)
	}
	if got := body(t, resp); got != "uploaded just now" {
		t.Errorf("served %q", got)
	}
}

// The case a stale walk would otherwise get silently wrong for a long time: a
// file replaced under its old name has a new pick code, and the cached listing
// still names the old one. 115 refusing it is the signal to refresh.
func TestAFileReplacedUnderTheSameNameHeals(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond, LinkTTL: 50 * time.Millisecond})

	if resp := do(t, srv, http.MethodGet, "/film.mkv", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up = %d", resp.StatusCode)
	}

	delete(b.blobs, "pc-film") // the old pick code is not downloadable any more
	b.blobs["pc-film2"] = []byte("the replacement")
	b.dirs["0"][1] = pan115.Entry{
		ID: "f1", Name: "film.mkv", Size: 15, PickCode: "pc-film2", SHA1: "replaced",
	}
	time.Sleep(80 * time.Millisecond)
	listed := b.listCalls.Load()

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the listing was refreshed", resp.StatusCode)
	}
	if got := body(t, resp); got != "the replacement" {
		t.Errorf("served %q, want the replacement", got)
	}
	if n := b.listCalls.Load() - listed; n != 1 {
		t.Errorf("refreshing cost %d listings, want 1 -- only the directory that named it", n)
	}
}

// A deleted directory is reported as missing rather than as an error. This one
// passes with or without the refresh -- a dead id and an absent name both end
// as 404 -- and is here for the status code, not the mechanism.
func TestADeletedDirectoryIsNoticed(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond})
	do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"})

	delete(b.dirs, "d1")          // the directory itself
	b.dirs["0"] = b.dirs["0"][1:] // and the entry that named it
	time.Sleep(80 * time.Millisecond)

	if resp := do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a directory that was deleted", resp.StatusCode)
	}
}

// Renaming changes neither an id nor a pick code, so a walk that reads the old
// name still lands on the right file. The old path keeps working until the
// listing is refreshed, which is harmless and is what makes a stale walk safe.
func TestRenamingDoesNotBreakACachedPath(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond, LinkTTL: time.Hour})
	do(t, srv, http.MethodGet, "/film.mkv", nil)

	renamed := b.dirs["0"][1]
	renamed.Name = "film (renamed).mkv"
	b.dirs["0"][1] = renamed
	time.Sleep(80 * time.Millisecond)

	if resp := do(t, srv, http.MethodGet, "/film.mkv", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("old path = %d, want 200 -- a rename keeps the pick code", resp.StatusCode)
	}
	if resp := do(t, srv, http.MethodGet, "/film (renamed).mkv", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("new path = %d, want 200 after the listing was refreshed", resp.StatusCode)
	}
}

// The deletion that does need the refresh: a directory made again under the
// same name has an id the cached listing has never seen. 115 answers a dead id
// by quietly serving the root, which pan115 turns into ErrNotFound -- and
// since the walk reads that listing however old it is, without refreshing the
// listing that named the old id the directory would stay unreachable.
func TestADirectoryRecreatedUnderTheSameNameHeals(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{DirTTL: 50 * time.Millisecond})
	do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"})

	delete(b.dirs, "d1")
	b.blobs["pc-other"] = []byte("new")
	b.dirs["d2"] = []pan115.Entry{{ID: "f8", Name: "other.mp4", Size: 3, PickCode: "pc-other"}}
	b.dirs["0"][0] = pan115.Entry{ID: "d2", Name: "Movies", IsDir: true}
	time.Sleep(80 * time.Millisecond)

	resp := do(t, srv, "PROPFIND", "/Movies", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207 once the listing naming the old id was refreshed", resp.StatusCode)
	}
	if page := body(t, resp); !strings.Contains(page, "other.mp4") {
		t.Error("the new directory's contents were not served")
	}
}
