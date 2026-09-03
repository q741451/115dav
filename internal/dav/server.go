package dav

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/q741451/115dav/internal/pan115"
)

// Options configures a Server.
type Options struct {
	// Backend is the 115 client.
	Backend Backend

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

	Logger *slog.Logger
}

// Server serves a 115 account over read-only WebDAV.
type Server struct {
	tree     *Tree
	stream   *streamer
	propfind *webdav.Handler
	opts     Options
	log      *slog.Logger
}

// New builds the server. It performs no I/O.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	tree := NewTree(opts.Backend, opts.RootID, opts.DirTTL)

	return &Server{
		tree:   tree,
		stream: newStreamer(opts.Backend, opts.LinkTTL, log),
		propfind: &webdav.Handler{
			FileSystem: &fileSystem{tree: tree},
			// Required by the handler even though locking is never exercised:
			// LOCK is rejected before it gets this far.
			LockSystem: webdav.NewMemLS(),
			Logger: func(r *http.Request, err error) {
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					log.Warn("propfind failed", "path", r.URL.Path, "err", err)
				}
			},
		},
		opts: opts,
		log:  log,
	}
}

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
		s.propfind.ServeHTTP(recorder, r)
	case http.MethodGet, http.MethodHead:
		s.serveContent(recorder, r)
	default:
		recorder.Header().Set("Allow", allowedMethods)
		http.Error(recorder, "read-only mount", http.StatusMethodNotAllowed)
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
	entry, err := s.tree.Lookup(r.Context(), r.URL.Path)
	if err != nil {
		s.replyLookupError(w, r, err)
		return
	}

	switch {
	case entry.IsDir:
		s.serveIndex(w, r, entry)
	case r.Method == http.MethodHead:
		s.stream.serveHead(w, entry)
	default:
		s.stream.serveGet(w, r, entry)
	}
}

func (s *Server) replyLookupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, pan115.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, pan115.ErrNotAuthorized):
		s.log.Error("115 rejected the session", "err", err)
		http.Error(w, "115 login expired", http.StatusBadGateway)
	default:
		s.log.Error("lookup failed", "path", r.URL.Path, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, entry pan115.Entry) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	children, err := s.tree.Children(r.Context(), entry.ID)
	if err != nil {
		s.replyLookupError(w, r, err)
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
