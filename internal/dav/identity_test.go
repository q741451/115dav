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

// Length is the one field the listing and the CDN both state, so it is where a
// stale listing can be caught. Serving the transfer anyway would put the new
// file's bytes behind the old file's ETag, modification time and the length a
// HEAD has already promised -- and nothing downstream could notice.
func TestAReplacedFileIsRefusedRatherThanMislabelled(t *testing.T) {
	for name, corrupt := range map[string]func(*fakeBackend){
		"the download endpoint reports another size": func(b *fakeBackend) {
			b.blobs["pc-film"] = []byte("a much shorter file than the listing claims")
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := sample(t)
			corrupt(b)
			srv := newTestServer(t, b, Options{})

			resp := do(t, srv, http.MethodGet, "/film.mkv", nil)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want %d -- a size the listing disagrees with was served",
					resp.StatusCode, http.StatusBadGateway)
			}
		})
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
