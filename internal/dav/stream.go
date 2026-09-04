package dav

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/q741451/115dav/internal/pan115"
)

// linkCache remembers resolved CDN targets. 115 hands out short-lived URLs, so
// the TTL is only an optimisation: expiry is ultimately detected by the CDN
// refusing a request, which invalidates the entry and forces a fresh resolve.
type linkCache struct {
	// owner bounds a resolve, for the same reason Tree has one: the URL is
	// shared, so it belongs to the credentials it was fetched with rather than
	// to the request that happened to ask first.
	owner   context.Context
	backend Backend
	ttl     time.Duration

	group singleflight.Group
	mu    sync.Mutex
	items map[string]cachedLink
}

type cachedLink struct {
	target  *pan115.Target
	expires time.Time
}

// maxCachedLinks bounds memory on a server that stays up for weeks. Each
// entry holds a CDN URL and the headers to replay with it, on the order of a
// kilobyte, and a library gets walked once and then never asked for again --
// so without a ceiling the map only ever grows.
const maxCachedLinks = 2048

// resolveTimeout bounds a resolve that no longer has a caller waiting for it.
const resolveTimeout = time.Minute

func newLinkCache(owner context.Context, backend Backend, ttl time.Duration) *linkCache {
	return &linkCache{owner: owner, backend: backend, ttl: ttl, items: map[string]cachedLink{}}
}

func (c *linkCache) get(ctx context.Context, pickCode string) (*pan115.Target, error) {
	if target := c.lookup(pickCode); target != nil {
		return target, nil
	}
	// Scrubbing a video fires many range requests at once; without this they
	// would each resolve the same pick code separately.
	resolved, err, _ := c.group.Do(pickCode, func() (any, error) {
		if target := c.lookup(pickCode); target != nil {
			return target, nil
		}
		// Detached from the caller for the same reason as a listing: the
		// resolve is shared, and the request that started it may hang up
		// while the others are still waiting on it.
		fetch, cancel := context.WithTimeout(c.owner, resolveTimeout)
		defer cancel()

		target, err := c.backend.Resolve(fetch, pickCode)
		if err != nil {
			return nil, err
		}
		c.store(pickCode, target)
		return target, nil
	})
	if err != nil {
		return nil, err
	}
	return resolved.(*pan115.Target), nil
}

func (c *linkCache) lookup(pickCode string) *pan115.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[pickCode]
	if !ok {
		return nil
	}
	if !time.Now().Before(item.expires) {
		// Drop it here rather than leaving it to pile up. Nothing else will:
		// forget() only runs when the CDN starts refusing a link.
		delete(c.items, pickCode)
		return nil
	}
	return item.target
}

func (c *linkCache) store(pickCode string, target *pan115.Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= maxCachedLinks {
		c.sweepLocked()
	}
	c.items[pickCode] = cachedLink{target: target, expires: time.Now().Add(c.ttl)}
}

// sweepLocked drops expired links, and everything if that was not enough. A
// cold cache costs one resolve per file, which is cheaper than an unbounded
// map on a device with little memory to spare.
func (c *linkCache) sweepLocked() {
	now := time.Now()
	for code, item := range c.items {
		if !now.Before(item.expires) {
			delete(c.items, code)
		}
	}
	if len(c.items) >= maxCachedLinks {
		clear(c.items)
	}
}

// forget drops a link, but only if the cache still holds the one that failed.
//
// The comparison is what stops a slow request from undoing a fast one. Several
// range requests on the same file are rejected at once when a URL expires;
// each re-resolves and stores, and without this check a straggler's forget
// would delete the good link a neighbour had just fetched, sending everyone
// back to the download endpoint -- which is rate limited to two calls a second
// and shared with every directory listing.
func (c *linkCache) forget(pickCode string, failed *pan115.Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if item, ok := c.items[pickCode]; ok && item.target == failed {
		delete(c.items, pickCode)
	}
}

// streamer proxies file bytes from the 115 CDN to the WebDAV client.
//
// Proxying rather than redirecting is deliberate: a CDN URL is tied to the
// User-Agent and session that requested it, which a player would not reproduce
// if it were sent there directly.
// The link cache is not here: it is derived from credentials and belongs to
// an epoch. What this holds instead lasts as long as the process, because none
// of it carries any 115 identity -- the cookies ride on each request, copied
// from the Target that was resolved for it.
type streamer struct {
	client *http.Client
	log    *slog.Logger
	bufs   sync.Pool
}

// streamBufferSize trades memory for syscalls on large sequential reads.
const streamBufferSize = 256 << 10

func newStreamer(log *slog.Logger) *streamer {
	return &streamer{
		// No Client.Timeout: a feature-length file legitimately streams for
		// hours. The transport bounds the parts that should be quick.
		//
		// Redirects use the standard policy, which keeps the User-Agent and
		// carries cookies only to the same registered domain.
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   8,
				ForceAttemptHTTP2:     true,
			},
		},
		log:  log,
		bufs: sync.Pool{New: func() any { b := make([]byte, streamBufferSize); return &b }},
	}
}

// identity is what a file is, as opposed to what one transfer of it looks
// like. PROPFIND, HEAD and GET must agree on every field here, so all three
// derive it from the same place -- the directory listing -- through this type.
//
// None of it is taken from the CDN, which reports its own ETag (a chunked MD5
// meaningful only to that CDN) and its own modification time (when the object
// reached that node, measured 52 seconds off the real one). A client that saw
// the listing's answer and then the CDN's has no way to tell it is looking at
// the same file.
type identity struct {
	size        int64
	etag        string
	modTime     time.Time
	contentType string
}

func identityOf(entry pan115.Entry) identity {
	return identity{
		size:        entry.Size,
		etag:        entryTag(entry),
		modTime:     entry.ModTime,
		contentType: contentType(entry.Name),
	}
}

// apply writes the fields that describe the file itself. Content-Length is not
// among them: it describes the response, and a 206 carries the length of the
// part rather than of the file.
func (id identity) apply(header http.Header) {
	header.Set("Content-Type", id.contentType)
	header.Set("ETag", id.etag)
	if !id.modTime.IsZero() {
		header.Set("Last-Modified", id.modTime.UTC().Format(http.TimeFormat))
	}
}

// serveHead answers from directory metadata alone. Players probe with HEAD
// before opening a stream, and there is no reason to spend a resolve on it.
func (s *streamer) serveHead(w http.ResponseWriter, entry pan115.Entry) {
	header := w.Header()
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	identityOf(entry).apply(header)
	w.WriteHeader(http.StatusOK)
}

// entryTag is the same value fileInfo.ETag reports through PROPFIND; both go
// through here so the two cannot drift.
func entryTag(entry pan115.Entry) string {
	if entry.SHA1 != "" {
		return strconv.Quote("sha1:" + entry.SHA1)
	}
	modTime := entry.ModTime
	if modTime.IsZero() {
		modTime = time.Unix(0, 0)
	}
	// The separator is load-bearing: without it (1, 0x23) and (0x12, 3) are
	// both "123", so two files could share a tag.
	return fmt.Sprintf(`"%x-%x"`, modTime.UnixNano(), entry.Size)
}

// pipe streams an already-opened upstream response to the client.
//
// Opening is done by the caller, inside the retry that can still replace the
// credentials; by the time anything gets here the response is committed.
func (s *streamer) pipe(w http.ResponseWriter, r *http.Request, entry pan115.Entry, id identity, resp *http.Response) error {
	defer resp.Body.Close()

	// The last point at which a disagreement can still be reported: nothing has
	// been written yet, so this can be a status code rather than a truncated
	// file the client would take for the real one.
	if err := id.checkLength(resp, entry.Name); err != nil {
		return err
	}

	copyResponseHeaders(w.Header(), resp.Header, id)
	w.WriteHeader(resp.StatusCode)

	buf := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(buf)

	// io.Copy reports a single error for both halves of the transfer, so wrap
	// the client side to find out which one failed. The distinction matters:
	// a player hanging up is routine, a CDN going quiet mid-file is not.
	client := &clientWriter{Writer: w}
	switch _, err := io.CopyBuffer(client, resp.Body, *buf); {
	case err == nil:
	case client.err != nil, r.Context().Err() != nil:
		// Players close connections constantly: after probing a container,
		// on every seek, and when playback stops. Which half of the copy
		// notices first is a race -- the write to the client fails, or the
		// cancelled request context aborts the read from the CDN -- so both
		// count as the player leaving.
		s.log.Debug("player closed the stream", "file", entry.Name, "err", err)
	default:
		// The status line is already out; all that is left is to record it.
		s.log.Warn("upstream stopped sending", "file", entry.Name, "err", err)
	}
	return nil
}

// clientWriter remembers the first failure writing to the client, so that it
// can be told apart from a failure reading from the CDN.
type clientWriter struct {
	io.Writer
	err error
}

func (c *clientWriter) Write(p []byte) (int, error) {
	n, err := c.Writer.Write(p)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

// open resolves the file and performs the upstream request, retrying once if
// the CDN rejects the cached URL or the connection fails outright.
func (s *streamer) open(r *http.Request, e *epoch, entry pan115.Entry, id identity) (*http.Response, error) {
	var lastErr error
	for attempt := range 2 {
		target, err := e.links.get(r.Context(), entry.PickCode)
		if err != nil {
			return nil, err
		}
		// The resolve reports the file's size too, and it was paid for
		// already. Checking it here catches a stale listing one round trip
		// before the CDN would, and without opening a connection.
		if target.Size > 0 && target.Size != id.size {
			return nil, &errStaleEntry{
				file: entry.Name, wanted: id.size, actual: target.Size,
				source: "the 115 download endpoint",
			}
		}

		resp, err := s.fetch(r, entry, target)
		switch {
		case err != nil:
			// The connection failed before any answer arrived. Nothing has
			// reached the client yet and a GET changes nothing upstream, so
			// trying again costs a round trip and saves a failed playback --
			// a home uplink drops connections for a living. The cached URL is
			// kept: it is the connection that failed, not the link.
			if r.Context().Err() != nil {
				return nil, err
			}
			lastErr = err
			if attempt == 0 {
				s.log.Debug("upstream connection failed, trying once more",
					"file", entry.Name, "err", err)
			}
		case !isExpired(resp.StatusCode):
			return resp, nil
		default:
			resp.Body.Close()
			lastErr = &upstreamError{status: resp.StatusCode, file: entry.Name}
			e.links.forget(entry.PickCode, target)
			if attempt == 0 {
				s.log.Debug("download link rejected, resolving again",
					"file", entry.Name, "status", resp.StatusCode)
			}
		}
	}
	return nil, lastErr
}

// rangeStillApplies evaluates If-Range against what we told the client this
// file was. A mismatch means the client is holding a stale copy, so it gets
// the whole file rather than a range spliced onto something else.
func rangeStillApplies(r *http.Request, entry pan115.Entry) bool {
	condition := strings.TrimSpace(r.Header.Get("If-Range"))
	if condition == "" {
		return true
	}
	if strings.HasPrefix(condition, `"`) || strings.HasPrefix(condition, "W/") {
		return strings.TrimPrefix(condition, "W/") == entryTag(entry)
	}
	// The other permitted form is a date, which must match exactly.
	stamp, err := http.ParseTime(condition)
	return err == nil && !entry.ModTime.IsZero() && stamp.Equal(entry.ModTime.UTC().Truncate(time.Second))
}

func (s *streamer) fetch(r *http.Request, entry pan115.Entry, target *pan115.Target) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = target.Header.Clone()
	// Seeking is the whole point: hand the range straight through and let the
	// CDN answer it, rather than reconstructing it locally.
	//
	// If-Range cannot be forwarded, because the validator in it is ours and
	// means nothing to the CDN, which would answer the whole file. It is
	// answered here instead: the condition holds when the file is unchanged,
	// and a 115 file's content hash cannot change without the entry changing.
	if value := r.Header.Get("Range"); value != "" && rangeStillApplies(r, entry) {
		req.Header.Set("Range", value)
	}
	return s.client.Do(req)
}

type upstreamError struct {
	status int
	file   string
}

func (e *upstreamError) Error() string {
	return "115 cdn refused the download link for " + e.file + ": HTTP " + strconv.Itoa(e.status)
}

// isExpired reports whether a CDN status means "this URL is no longer good"
// rather than "this request was wrong".
func isExpired(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return true
	}
	return false
}

// passthroughHeaders are copied from the CDN response as-is: they describe
// this transfer, which only the CDN knows about. Everything that describes the
// file itself comes from the listing instead, so that it matches what PROPFIND
// said. Everything else, notably Set-Cookie and any 115 session state, is
// dropped.
var passthroughHeaders = []string{
	"Content-Length",
	"Content-Range",
}

func copyResponseHeaders(dst, src http.Header, id identity) {
	for _, name := range passthroughHeaders {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
	dst.Set("Accept-Ranges", "bytes")
	id.apply(dst)

	// The CDN labels nearly everything as a generic binary stream, so our own
	// guess is used above; fall back to its label only when we have none.
	if id.contentType == defaultContentType {
		if value := src.Get("Content-Type"); value != "" {
			dst.Set("Content-Type", value)
		}
	}
}

// errStaleEntry reports that the file the CDN is about to send is not the one
// the listing described.
type errStaleEntry struct {
	file           string
	wanted, actual int64
	source         string
}

func (e *errStaleEntry) Error() string {
	return fmt.Sprintf("%s says %s is %d bytes, but the listing this request was answered from says %d;"+
		" the cached directory is stale", e.source, e.file, e.actual, e.wanted)
}

// checkLength refuses a transfer whose size contradicts the identity already
// promised for this file.
//
// Length is the one field that both sides state, so it is the one place the
// two sources of truth can be caught disagreeing. They disagree when a file
// has been replaced under the same name since the directory was listed, and
// serving it anyway would send the new file's bytes under the old file's ETag,
// modification time and -- for a HEAD already answered -- length. Refusing
// costs this one request; the listing expires and the next one is right.
func (id identity) checkLength(resp *http.Response, file string) error {
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// "bytes 100-199/21981": only the total is ours to check. A "*" total
		// is legal and says the CDN does not know it, which is not a conflict.
		_, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if !ok || total == "*" {
			return nil
		}
		size, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
		if err != nil || size == id.size {
			return nil
		}
		return &errStaleEntry{file: file, wanted: id.size, actual: size, source: "the CDN's Content-Range"}
	case http.StatusOK:
		if resp.ContentLength < 0 || resp.ContentLength == id.size {
			return nil
		}
		return &errStaleEntry{file: file, wanted: id.size, actual: resp.ContentLength, source: "the CDN's Content-Length"}
	}
	return nil
}

const defaultContentType = "application/octet-stream"

// mediaTypes covers what the Go mime table misses. Container formats matter
// most: players use the type to decide whether a file is worth opening.
var mediaTypes = map[string]string{
	".mkv": "video/x-matroska", ".mp4": "video/mp4", ".m4v": "video/x-m4v",
	".avi": "video/x-msvideo", ".mov": "video/quicktime", ".wmv": "video/x-ms-wmv",
	".flv": "video/x-flv", ".webm": "video/webm", ".mpg": "video/mpeg",
	".mpeg": "video/mpeg", ".ts": "video/mp2t", ".m2ts": "video/mp2t",
	".rmvb": "application/vnd.rn-realmedia-vbr", ".iso": "application/x-iso9660-image",
	".m3u8": "application/vnd.apple.mpegurl",

	".mp3": "audio/mpeg", ".flac": "audio/flac", ".aac": "audio/aac",
	".m4a": "audio/mp4", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".opus": "audio/opus", ".ape": "audio/x-ape", ".wma": "audio/x-ms-wma",

	".srt": "application/x-subrip", ".ass": "text/x-ssa", ".ssa": "text/x-ssa",
	".vtt": "text/vtt", ".sub": "text/plain", ".idx": "text/plain",
	".nfo": "text/plain",
}

func contentType(name string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return defaultContentType
}
