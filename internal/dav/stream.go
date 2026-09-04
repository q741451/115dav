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

// maxDiscardedBody is how much of a refused response is read before giving up
// on reusing its connection.
const maxDiscardedBody = 4 << 10

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
		if target == nil {
			// No implementation does this, but the alternative to checking is
			// caching a nil and dereferencing it one call later, in a place
			// with no clue as to where it came from.
			return nil, fmt.Errorf("115 returned no download target for pick code %s", pickCode)
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
//
// The link cache is not here: it is derived from credentials and belongs to
// an epoch. What this holds instead lasts as long as the process, because none
// of it carries any 115 identity -- the cookies ride on each request, copied
// from the Target that was resolved for it.
type streamer struct {
	client *http.Client
	slots  *slots
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
				// Both caps matter. 115 hands out CDN hostnames that vary by
				// file and by region, so a per-host limit alone bounds nothing
				// in total: without MaxIdleConns a library walk can leave an
				// idle connection, and a descriptor, for every host it touched
				// until the idle timeout reaps them. Zero would mean no limit.
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				ForceAttemptHTTP2:   true,
			},
		},
		slots: newSlots(log),
		log:   log,
		bufs:  sync.Pool{New: func() any { b := make([]byte, streamBufferSize); return &b }},
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
// credentials; by the time anything gets here the response is committed, which
// is why this reports nothing. The status line has gone out, so there is no
// answer left to give -- only a log line.
//
// The body is closed rather than drained. Draining would let HTTP/1.1 keep the
// connection, which is worth about 340 ms on the next read of that file, but
// what remains of an interrupted playback is the rest of the film. Reads that
// finish are already reused; this is the one case where the two goals are
// opposed, and the bandwidth wins.
func (s *streamer) pipe(w http.ResponseWriter, r *http.Request, mine *slot, entry pan115.Entry, id identity, resp *http.Response) {
	defer resp.Body.Close()

	if size, differs := id.actualSize(resp); differs {
		s.log.Debug("115 lists this file at the wrong size; serving what the CDN sends",
			"file", entry.Name, "listed", id.size, "actual", size)
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
	case s.slots.evicted(mine):
		// Someone asked for this file while it was already being served the
		// most times 115 will allow, and this was the older stream. The player
		// sees a truncated response and will usually reconnect, which is the
		// accepted cost of never having to guess whether a quiet stream is
		// still wanted.
		s.log.Info("stream cut short to admit a newer request for the same file",
			"file", entry.Name, "limit", s.slots.limit)
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
func (s *streamer) open(ctx context.Context, r *http.Request, e *epoch, entry pan115.Entry, id identity) (*http.Response, error) {
	var (
		lastErr   error
		throttles int
	)
	// Being throttled does not spend an attempt: the two failures are
	// unrelated, and a link that has to be resolved again after a wait would
	// otherwise have no attempt left to use it.
	for attempt := 0; attempt < 2; {
		target, err := e.links.get(ctx, entry.PickCode)
		if err != nil {
			return nil, err
		}
		resp, err := s.fetch(ctx, r, entry, target)
		throttled, expired := false, false
		if err == nil {
			throttled, expired = classify(resp)
		}
		switch {
		case err != nil:
			// The connection failed before any answer arrived. Nothing has
			// reached the client yet and a GET changes nothing upstream, so
			// trying again costs a round trip and saves a failed playback --
			// a home uplink drops connections for a living. The cached URL is
			// kept: it is the connection that failed, not the link.
			if ctx.Err() != nil {
				return nil, cancelled(ctx, r, entry, err)
			}
			lastErr = err
			if attempt == 0 {
				s.log.Debug("upstream connection failed, trying once more",
					"file", entry.Name, "err", err)
			}
		case throttled:
			// The link is good and is kept. Only the timing was wrong.
			io.CopyN(io.Discard, resp.Body, maxDiscardedBody)
			resp.Body.Close()
			lastErr = &upstreamError{status: resp.StatusCode, file: entry.Name, throttled: true}
			if throttles++; throttles > maxThrottleRetries {
				return nil, lastErr
			}
			s.log.Debug("115 is already serving this file as often as it allows, waiting",
				"file", entry.Name, "attempt", throttles)
			wait := time.NewTimer(throttleBackoff)
			select {
			case <-ctx.Done():
				wait.Stop()
				return nil, cancelled(ctx, r, entry, ctx.Err())
			case <-wait.C:
			}
			continue
		case !expired:
			return resp, nil
		default:
			// Drained, not just closed. HTTP/1.1 cannot reuse a connection
			// whose body was abandoned, so closing on its own tears the
			// connection down and the retry below has to dial again -- on a
			// path that exists precisely because a seek is in progress. The
			// bodies here are short refusals; the cap is what stops a
			// misbehaving upstream turning this into a download.
			io.CopyN(io.Discard, resp.Body, maxDiscardedBody)
			resp.Body.Close()
			lastErr = &upstreamError{status: resp.StatusCode, file: entry.Name}
			e.links.forget(entry.PickCode, target)
			if attempt == 0 {
				s.log.Debug("download link rejected, resolving again",
					"file", entry.Name, "status", resp.StatusCode)
			}
		}
		attempt++
	}
	return nil, lastErr
}

// cancelled says why a read stopped, for the two places that can notice.
//
// A cancelled stream context means one of two things, and only the request's
// own context tells them apart: the client went away, which is routine and
// needs no answer, or this read was cut here to admit a newer one of the same
// file, which the client did not ask for and should retry. Reporting the
// second as a plain cancellation makes it a bad gateway -- blaming 115 for a
// decision made here, and inviting the player to give up on a file that is
// perfectly fine.
func cancelled(ctx context.Context, r *http.Request, entry pan115.Entry, err error) error {
	if r.Context().Err() == nil {
		return &upstreamError{file: entry.Name, evicted: true}
	}
	return err
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

func (s *streamer) fetch(ctx context.Context, r *http.Request, entry pan115.Entry, target *pan115.Target) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
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
	// throttled: 115 refused because the file is already being served as many
	// times at once as it allows. evicted: this read was cut here, to admit a
	// newer one of the same file. Both are this moment rather than this file,
	// so both are answered 503 and both are worth retrying.
	throttled bool
	evicted   bool
}

func (e *upstreamError) Error() string {
	switch {
	case e.evicted:
		return "the read of " + e.file + " was cut short to admit a newer one of the same file"
	case e.throttled:
		return "115 is already serving " + e.file + " as many times at once as it allows"
	}
	return "115 cdn refused the download link for " + e.file + ": HTTP " + strconv.Itoa(e.status)
}

// temporary reports whether asking again shortly is worth the client's while.
func (e *upstreamError) temporary() bool { return e.throttled || e.evicted }

// A 403 from the CDN means one of two opposite things, and the body is what
// tells them apart.
//
//	{"status":403,"message":"115 pmt 3-2","request_id":"..."}
//
// A "pmt" refusal is the file being served as many times at once as 115 will
// allow. The URL is fine; asking again in a moment works. Throwing it away
// would spend one of two calls a second on the download endpoint to be handed
// back the same URL -- and if that happened on every seek, most of the rate
// limit would go on re-fetching links that were never stale.
//
// Any other 403, and 401 and 410, mean the URL itself is finished, and the
// only way forward is to resolve again.
const throttleMarker = "pmt"

func classify(resp *http.Response) (throttled, expired bool) {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusGone:
		return false, true
	case http.StatusForbidden:
		// The refusal is a short JSON body; read enough to recognise it and
		// drain the rest so the connection can be reused.
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if strings.Contains(string(prefix), throttleMarker) {
			return true, false
		}
		return false, true
	}
	return false, false
}

// throttleBackoff is how long to wait before asking again after a "pmt"
// refusal.
//
// It exists because evicting a stream is not instant end to end: cancelling
// aborts our request immediately, but 115 stops counting it only once the
// connection teardown reaches it, and how long that takes is not something
// this side can see. Rather than guess at a delay before every request, the
// refusal itself is the signal -- which also covers the slots this process
// cannot know about, such as the same file being played on another device.
var throttleBackoff = 400 * time.Millisecond

// maxThrottleRetries bounds the wait. Past it the file really is busy, and
// saying so beats holding a player open indefinitely.
const maxThrottleRetries = 3

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

// actualSize reports the length the CDN is about to send when it differs from
// the listing's.
//
// 115 lists some old files at the wrong size -- one byte for a four hundred
// megabyte video, with its download endpoint repeating the same number. Only
// the transfer knows, so a disagreement is not a reason to refuse: the
// response already carries the CDN's own Content-Length and Content-Range, and
// this is here so that -v explains why PROPFIND said something else.
func (id identity) actualSize(resp *http.Response) (int64, bool) {
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// "bytes 100-199/21981": only the total says anything about the file.
		// A "*" total is legal and means the CDN does not know it either.
		_, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if !ok || total == "*" {
			return 0, false
		}
		size, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
		if err != nil || size == id.size {
			return 0, false
		}
		return size, true
	case http.StatusOK:
		if resp.ContentLength < 0 || resp.ContentLength == id.size {
			return 0, false
		}
		return resp.ContentLength, true
	}
	return 0, false
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
