package dav

import (
	"context"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// Options configures a Server.
type Options struct {
	// Credentials supplies the 115 client. Static wraps one that never
	// changes; the cookie-sync session replaces its own.
	Credentials Credentials

	// RootID is the 115 category id to mount. Empty means the account root.
	RootID string

	// DirTTL is how long a directory listing may be served from cache.
	DirTTL time.Duration

	// LinkTTL bounds how long a resolved CDN URL is reused. Expiry is also
	// detected from the CDN itself, so this is a hint rather than a deadline.
	LinkTTL time.Duration

	// Username and Password enable HTTP Basic authentication when both are
	// set. Leaving them empty serves the mount without authentication.
	Username, Password string

	// RetryAfter is advertised alongside a 503.
	RetryAfter time.Duration

	Logger *slog.Logger
}

// Server serves a 115 account over read-only WebDAV.
type Server struct {
	epochs *epochs
	stream *streamer
	opts   Options
	log    *slog.Logger
}

// New builds the server. It performs no I/O.
//
// The context bounds every piece of shared work the server will start:
// cancelling it retires the current epoch, which cancels the listings and
// resolves begun under it. Serving one client's bytes is not shared work and
// runs on that request's own context, so a shutdown does not cut a stream
// short before the HTTP server has had its chance to drain.
func New(ctx context.Context, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		epochs: &epochs{owner: ctx, creds: opts.Credentials, opts: opts, log: log},
		stream: newStreamer(log),
		opts:   opts,
		log:    log,
	}
}

// Close retires the current epoch, cancelling the shared work it owns.
func (s *Server) Close() { s.epochs.close() }

// allowedMethods is what a read-only mount supports, and what OPTIONS
// advertises. Anything outside it is refused with 405.
const allowedMethods = "OPTIONS, GET, HEAD, PROPFIND"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path, "range", r.Header.Get("Range"),
			"status", recorder.status, "took", time.Since(started).Round(time.Millisecond))
	}()

	if !s.authorised(r) {
		recorder.Header().Set("WWW-Authenticate", `Basic realm="115", charset="UTF-8"`)
		http.Error(recorder, "unauthorised", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		s.serveOptions(recorder)
	case "PROPFIND":
		s.servePropfind(recorder, r)
	case http.MethodGet, http.MethodHead:
		s.serveContent(recorder, r)
	default:
		recorder.Header().Set("Allow", allowedMethods)
		http.Error(recorder, "read-only mount", http.StatusMethodNotAllowed)
	}
}

// servePropfind answers a listing.
//
// Everything the response will mention is resolved before a byte of it is
// written; see the note at the top of propfind.go for why that ordering is the
// whole design and not an optimisation.
func (s *Server) servePropfind(w http.ResponseWriter, r *http.Request) {
	depth := parseDepth(r.Header.Get("Depth"))
	switch depth {
	case depthInfinite:
		// RFC 4918 allows refusing an unbounded walk, and here it has to be
		// refused: it would list the entire account, one rate-limited request
		// per directory, for as long as that takes. Clients are expected to
		// handle this and ask again with a depth.
		s.log.Warn("refused an unbounded PROPFIND", "path", r.URL.Path, "agent", r.UserAgent())
		w.Header().Set("Content-Type", xmlContentType)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, xml.Header+`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
		return
	case depthInvalid:
		// A client's mistake rather than ours, so it is not a warning -- but
		// worth having when a player is behaving oddly.
		s.log.Debug("refused a PROPFIND with an unusable Depth",
			"path", r.URL.Path, "depth", r.Header.Get("Depth"), "agent", r.UserAgent())
		http.Error(w, "invalid depth", http.StatusBadRequest)
		return
	}

	req, err := parsePropfind(r.Body)
	if err != nil {
		s.log.Warn("could not read the PROPFIND body", "path", r.URL.Path, "err", err)
		http.Error(w, "invalid PROPFIND body", http.StatusBadRequest)
		return
	}

	snap, err := withEpoch(s.epochs, r.Context(), func(e *epoch) (snapshot, error) {
		return e.snapshot(r.Context(), r.URL.Path, depth)
	})
	if err != nil {
		s.replyError(w, r, pan115.Entry{}, err)
		return
	}

	w.Header().Set("Content-Type", xmlContentType)
	w.WriteHeader(http.StatusMultiStatus)
	if err := writeMultistatus(w, snap, req); err != nil {
		// Only the client going away can get here; the body was already built.
		s.log.Debug("propfind write failed", "path", r.URL.Path, "err", err)
	}
}

// xmlContentType labels every XML body this server sends.
//
// text/xml rather than application/xml: RFC 4918 permits either, this is what
// the multistatus has always carried, and some older clients look for it. The
// point is that there is one answer -- the error bodies used to disagree with
// the multistatus for no reason anybody chose.
const xmlContentType = "text/xml; charset=utf-8"

// Depth values, matching what x/net/webdav accepts. A missing header means
// infinity, per RFC 4918.
const (
	depthInfinite = -1
	depthInvalid  = -2
)

func parseDepth(value string) int {
	switch value {
	case "0":
		return 0
	case "1":
		return 1
	case "", "infinity":
		return depthInfinite
	default:
		return depthInvalid
	}
}

func (s *Server) serveOptions(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Allow", allowedMethods)
	header.Set("DAV", "1")
	header.Set("MS-Author-Via", "DAV")
	header.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// serveContent handles GET and HEAD, routing files to the streaming proxy and
// directories to a plain index that is only there for eyeballing the mount.
func (s *Server) serveContent(w http.ResponseWriter, r *http.Request) {
	// Looking the path up and opening the upstream response happen inside one
	// retry, for two reasons. A login that expired is usually discovered at the
	// resolve rather than the lookup -- listings are cached for minutes while
	// resolves happen constantly -- so a retry that covered only the lookup
	// would almost never fire. And nothing has been written to the client at
	// this point, so replacing the credentials and starting again is still
	// possible. Past here it is not.
	got, err := withEpoch(s.epochs, r.Context(), func(e *epoch) (content, error) {
		entry, err := e.tree.Lookup(r.Context(), r.URL.Path)
		if err != nil || entry.IsDir || r.Method == http.MethodHead {
			return content{epoch: e, entry: entry}, err
		}
		id := identityOf(entry)
		resp, err := s.stream.open(r, e, entry, id)
		return content{epoch: e, entry: entry, id: id, resp: resp}, err
	})
	if err != nil {
		s.replyError(w, r, got.entry, err)
		return
	}

	switch {
	case got.entry.IsDir:
		s.serveIndex(w, r, got.epoch, got.entry)
	case r.Method == http.MethodHead:
		s.stream.serveHead(w, got.entry)
	default:
		if err := s.stream.pipe(w, r, got.entry, got.id, got.resp); err != nil {
			s.replyError(w, r, got.entry, err)
		}
	}
}

// content is everything one GET or HEAD needs, gathered under a single set of
// credentials.
type content struct {
	epoch *epoch
	entry pan115.Entry
	id    identity
	resp  *http.Response // nil for HEAD and for directories
}

func (s *Server) serveUnavailable(w http.ResponseWriter) {
	writeUnavailable(w, s.opts.RetryAfter)
}

// writeUnavailable answers 503 with a hint about when to come back. Anything
// but an empty success will do here; see ErrUnavailable for why an empty
// listing would be the wrong answer.
func writeUnavailable(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	http.Error(w, "115 credentials are being refreshed", http.StatusServiceUnavailable)
}

// replyError turns a failure into a status code. It is the single place that
// decides, so that a lookup and a stream cannot disagree about what, say, an
// expired login means.
//
// The entry may be zero, when the failure happened before the path resolved.
func (s *Server) replyError(w http.ResponseWriter, r *http.Request, entry pan115.Entry, err error) {
	// A cancelled request context is how a client hanging up reaches us, and is
	// nothing to report. Only this request's own context counts: a cancellation
	// carried in err may have come from another caller that shared the work,
	// and answering that with silence would leave a client that is still
	// waiting holding an empty 200.
	if r.Context().Err() != nil {
		return
	}

	var notDownloadable *pan115.ErrNotDownloadable
	switch {
	case errors.Is(err, errNoCredentials), errors.Is(err, ErrUnavailable):
		s.serveUnavailable(w)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, pan115.ErrNotFound):
		http.NotFound(w, r)
	case errors.As(err, &notDownloadable):
		s.log.Error("115 will not serve this file", "path", r.URL.Path, "err", err)
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	case errors.Is(err, pan115.ErrNotAuthorized):
		s.log.Error("115 rejected the session", "err", err)
		http.Error(w, "115 login expired", http.StatusBadGateway)
	default:
		s.log.Error("cannot serve", "path", r.URL.Path, "file", entry.Name, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, e *epoch, entry pan115.Entry) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	children, err := e.tree.Children(r.Context(), entry.ID)
	if err != nil {
		s.replyError(w, r, pan115.Entry{}, err)
		return
	}

	base := r.URL.Path
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	// Built by hand rather than with html/template: executing a template
	// drags the whole reflect-based template engine into the binary, which
	// costs more than two megabytes for what is one debugging page. Every
	// interpolation below is escaped explicitly instead.
	var page strings.Builder
	title := html.EscapeString(path.Clean("/" + base))
	page.WriteString(`<!doctype html><meta charset="utf-8"><title>`)
	page.WriteString(title)
	page.WriteString(`</title><style>body{font:14px system-ui;margin:2rem}td{padding:.15rem 1rem .15rem 0}
td.s{text-align:right;color:#666;font-variant-numeric:tabular-nums}</style><h1>`)
	page.WriteString(title)
	page.WriteString("</h1><table>\n")
	for _, child := range children {
		name := child.Name
		if child.IsDir {
			name += "/"
		}
		href := (&url.URL{Path: base + child.Name}).EscapedPath()
		page.WriteString(`<tr><td><a href="` + html.EscapeString(href) + `">` +
			html.EscapeString(name) + `</a></td><td class="s">` +
			humanSize(child) + // digits and a unit, nothing to escape
			"</td></tr>\n")
	}
	page.WriteString("</table>\n")
	if _, err := io.WriteString(w, page.String()); err != nil {
		s.log.Debug("index write failed", "err", err)
	}
}

func humanSize(entry pan115.Entry) string {
	if entry.IsDir {
		return ""
	}
	const unit = 1024
	if entry.Size < unit {
		return fmt.Sprintf("%d B", entry.Size)
	}
	size := float64(entry.Size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		if size /= unit; size < unit {
			return fmt.Sprintf("%.1f %s", size, suffix)
		}
	}
	return fmt.Sprintf("%.0f EiB", size/unit)
}

// authorised reports whether the request may proceed. Comparisons are constant
// time so a wrong password cannot be found a character at a time.
func (s *Server) authorised(r *http.Request) bool {
	if s.opts.Username == "" && s.opts.Password == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.opts.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.opts.Password)) == 1
	return userOK && passOK
}

// statusRecorder remembers the status line for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if !w.written {
		w.status, w.written = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}
