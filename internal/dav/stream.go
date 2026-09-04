package dav

import (
	"context"
	"errors"
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

func newLinkCache(backend Backend, ttl time.Duration) *linkCache {
	return &linkCache{backend: backend, ttl: ttl, items: map[string]cachedLink{}}
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
		fetch, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolveTimeout)
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

// clear empties the cache, for when the credentials behind every cached link
// have been replaced.
func (c *linkCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.items)
}

func (c *linkCache) forget(pickCode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, pickCode)
}

// streamer proxies file bytes from the 115 CDN to the WebDAV client.
//
// Proxying rather than redirecting is deliberate: a CDN URL is tied to the
// User-Agent and session that requested it, which a player would not reproduce
// if it were sent there directly.
type streamer struct {
	links      *linkCache
	client     *http.Client
	log        *slog.Logger
	retryAfter time.Duration
	bufs       sync.Pool
}

// streamBufferSize trades memory for syscalls on large sequential reads.
const streamBufferSize = 256 << 10

func newStreamer(backend Backend, linkTTL, retryAfter time.Duration, log *slog.Logger) *streamer {
	return &streamer{
		links: newLinkCache(backend, linkTTL),
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
		log:        log,
		retryAfter: retryAfter,
		bufs:       sync.Pool{New: func() any { b := make([]byte, streamBufferSize); return &b }},
	}
}

// serveHead answers from directory metadata alone. Players probe with HEAD
// before opening a stream, and there is no reason to spend a resolve on it.
func (s *streamer) serveHead(w http.ResponseWriter, entry pan115.Entry) {
	header := w.Header()
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	setIdentity(header, entry)
	w.WriteHeader(http.StatusOK)
}

// setIdentity applies the metadata that must be the same however the file was
// asked about. The CDN answers with its own ETag and its own idea of the
// modification time, both different from what PROPFIND reported for the same
// file; a client that saw one and then the other has no way to tell it is
// looking at the same thing.
func setIdentity(header http.Header, entry pan115.Entry) {
	header.Set("Content-Type", contentType(entry.Name))
	header.Set("ETag", entryTag(entry))
	if !entry.ModTime.IsZero() {
		header.Set("Last-Modified", entry.ModTime.UTC().Format(http.TimeFormat))
	}
}

// entryTag is the same value fileInfo.ETag reports through PROPFIND.
func entryTag(entry pan115.Entry) string {
	if entry.SHA1 != "" {
		return strconv.Quote("sha1:" + entry.SHA1)
	}
	modTime := entry.ModTime
	if modTime.IsZero() {
		modTime = time.Unix(0, 0)
	}
	return fmt.Sprintf(`"%x%x"`, modTime.UnixNano(), entry.Size)
}

// serveGet streams the file, forwarding the client's range request upstream.
func (s *streamer) serveGet(w http.ResponseWriter, r *http.Request, entry pan115.Entry) {
	resp, err := s.open(r, entry)
	if err != nil {
		s.fail(w, r, entry, err)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header, entry)
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
func (s *streamer) open(r *http.Request, entry pan115.Entry) (*http.Response, error) {
	var lastErr error
	for attempt := range 2 {
		target, err := s.links.get(r.Context(), entry.PickCode)
		if err != nil {
			return nil, err
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
			s.links.forget(entry.PickCode)
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

func (s *streamer) fail(w http.ResponseWriter, r *http.Request, entry pan115.Entry, err error) {
	// A cancelled request context is how a client hanging up reaches us, and
	// is nothing to report. Only this request's own context counts: a
	// cancellation carried in err may have come from another caller that
	// shared the work, and answering that with silence would leave a client
	// that is still waiting holding an empty 200.
	if r.Context().Err() != nil {
		return
	}
	if !errors.Is(err, ErrUnavailable) {
		s.log.Error("cannot stream file", "file", entry.Name, "err", err)
	}

	var notDownloadable *pan115.ErrNotDownloadable
	switch {
	case errors.Is(err, ErrUnavailable):
		writeUnavailable(w, s.retryAfter)
	case errors.As(err, &notDownloadable):
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	default:
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}
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

func copyResponseHeaders(dst, src http.Header, entry pan115.Entry) {
	for _, name := range passthroughHeaders {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
	dst.Set("Accept-Ranges", "bytes")
	setIdentity(dst, entry)

	// The CDN labels nearly everything as a generic binary stream, so our own
	// guess is used above; fall back to its label only when we have none.
	if contentType(entry.Name) == defaultContentType {
		if value := src.Get("Content-Type"); value != "" {
			dst.Set("Content-Type", value)
		}
	}
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
