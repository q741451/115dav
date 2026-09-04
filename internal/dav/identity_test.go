package dav

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// Every way of asking about a file must describe the same file. A player that
// gets one ETag from PROPFIND and another from GET cannot tell that its cached
// range still belongs to what it is playing, and re-fetches or stalls.
func TestOneFileHasOneIdentity(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	propfind := do(t, srv, "PROPFIND", "/film.mkv", map[string]string{"Depth": "0"})
	multistatus := body(t, propfind)

	for name, resp := range map[string]*http.Response{
		"HEAD":  do(t, srv, http.MethodHead, "/film.mkv", nil),
		"GET":   do(t, srv, http.MethodGet, "/film.mkv", nil),
		"range": do(t, srv, http.MethodGet, "/film.mkv", map[string]string{"Range": "bytes=10-19"}),
	} {
		t.Run(name, func(t *testing.T) {
			etag := resp.Header.Get("ETag")
			if etag == "" {
				t.Fatal("no ETag")
			}
			if !strings.Contains(multistatus, etag) {
				t.Errorf("ETag %s is not the one PROPFIND published", etag)
			}
			if got := resp.Header.Get("Last-Modified"); !strings.Contains(multistatus, got) {
				t.Errorf("Last-Modified %q is not the one PROPFIND published", got)
			}
			if got := resp.Header.Get("Content-Type"); !strings.Contains(multistatus, got) {
				t.Errorf("Content-Type %q is not the one PROPFIND published", got)
			}
		})
	}
}

// 115 lists some old files at a size that is simply wrong -- one byte for a
// four hundred megabyte video, its download endpoint agreeing with the listing
// and only the transfer knowing better. Refusing on the disagreement made
// every such file unplayable, so the CDN's bytes win and the file plays.
func TestAFileListedAtTheWrongSizeStillPlays(t *testing.T) {
	b := sample(t)
	// The listing keeps the size it was built with; the file behind it is a
	// different length, as 115's malformed entries are.
	b.blobs["pc-film"] = []byte(strings.Repeat("x", 40000))
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- a wrong listed size must not make a file unplayable", resp.StatusCode)
	}
	// And what arrives is the whole file, described by the CDN's length rather
	// than the listing's.
	got := body(t, resp)
	if len(got) != len(b.blobs["pc-film"]) {
		t.Errorf("served %d bytes of %d", len(got), len(b.blobs["pc-film"]))
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(got)) {
		t.Errorf("Content-Length = %q for a %d byte body", cl, len(got))
	}
}

// A range of such a file works too, which is what a player actually asks for.
func TestARangeOfAFileListedAtTheWrongSizeWorks(t *testing.T) {
	b := sample(t)
	b.blobs["pc-film"] = []byte(strings.Repeat("y", 40000))
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, http.MethodGet, "/film.mkv", map[string]string{"Range": "bytes=100-199"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if n := len(body(t, resp)); n != 100 {
		t.Errorf("got %d bytes, want 100", n)
	}
}

// HEAD promises a length from the listing alone. Whatever GET then answers has
// to be the same number, or the client has been lied to by one of them.
func TestHeadAndGetAgreeOnLength(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	head := do(t, srv, http.MethodHead, "/film.mkv", nil)
	get := do(t, srv, http.MethodGet, "/film.mkv", nil)

	if head.Header.Get("Content-Length") != get.Header.Get("Content-Length") {
		t.Errorf("HEAD says %q bytes, GET says %q",
			head.Header.Get("Content-Length"), get.Header.Get("Content-Length"))
	}
	if n := len(body(t, get)); head.Header.Get("Content-Length") != strconv.Itoa(n) {
		t.Errorf("HEAD promised %q bytes and GET delivered %d", head.Header.Get("Content-Length"), n)
	}
}

// Without a separator the tag is the concatenation of two hex numbers, so
// (mtime 0x1, size 0x23) and (mtime 0x12, size 0x3) both render as "123".
// Files without a SHA1 are the only ones that reach this path, and two of them
// sharing a tag would let a client splice one into the other.
func TestFallbackTagsDoNotCollide(t *testing.T) {
	first := entryTag(pan115.Entry{ModTime: time.Unix(0, 0x1), Size: 0x23})
	second := entryTag(pan115.Entry{ModTime: time.Unix(0, 0x12), Size: 0x3})
	if first == second {
		t.Errorf("two different files share the tag %s", first)
	}
}

// The content hash is preferred wherever 115 has one, because it survives a
// re-listing, a restart and a different CDN node.
func TestTagPrefersTheContentHash(t *testing.T) {
	tag := entryTag(pan115.Entry{SHA1: "ABC", ModTime: time.Unix(1, 0), Size: 7})
	if tag != `"sha1:ABC"` {
		t.Errorf("tag = %s, want the sha1", tag)
	}
}
