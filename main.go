// Command 115dav serves a 115 account as a read-only WebDAV mount, so that
// players such as Infuse can stream from it directly.
//
// File bytes are proxied rather than redirected: 115 ties each download URL to
// the User-Agent and session that asked for it, which a player would not
// reproduce if it were pointed at the CDN itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/q741451/115dav/internal/cookiesync"
	"github.com/q741451/115dav/internal/dav"
	"github.com/q741451/115dav/internal/pan115"
	"github.com/q741451/115dav/internal/session"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// Builds made outside the release workflow keep the placeholder.
var version = "dev"

type options struct {
	listen      string
	cookie      string
	cookieFile  string
	root        string
	username    string
	password    string
	dirTTL      time.Duration
	linkTTL     time.Duration
	rate        float64
	pageSize    int
	userAgent   string
	verbose     bool
	showVersion bool

	// Subscription mode: read the cookie from a cookie-sync server instead of
	// being given one. Mutually exclusive with the options above.
	cookieServer  string
	cookieChannel string
	cookieKey     string
	cookieKeyFile string
	cookieDomain  string
	cookieRetry   time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "115dav:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := parseFlags()
	if opts.showVersion {
		fmt.Printf("115dav %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}

	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := checkModes(opts); err != nil {
		return err
	}

	build := func(cookie string) (*pan115.Client, error) {
		return pan115.New(pan115.Config{
			Cookie:            cookie,
			UserAgent:         opts.userAgent,
			RequestsPerSecond: opts.rate,
			PageSize:          opts.pageSize,
		})
	}

	var (
		backend     dav.Backend
		unavailable func() bool
		onServer    func(*dav.Server)
		err         error
	)
	if opts.cookieServer != "" {
		var synced *session.Session
		if synced, err = openSynced(opts, build, log); err == nil {
			backend, unavailable = synced, synced.Blocked
			onServer = func(s *dav.Server) { synced.OnRefresh(s.Flush) }
		}
	} else {
		backend, err = openStatic(opts, build, log)
	}
	if err != nil {
		return err
	}

	handler := dav.New(dav.Options{
		Backend:     backend,
		RootID:      opts.root,
		DirTTL:      opts.dirTTL,
		LinkTTL:     opts.linkTTL,
		Username:    opts.username,
		Password:    opts.password,
		Unavailable: unavailable,
		RetryAfter:  opts.cookieRetry,
		Logger:      log,
	})
	if onServer != nil {
		onServer(handler)
	}

	server := &http.Server{
		Addr:    opts.listen,
		Handler: handler,
		// No WriteTimeout: a write deadline would cut off long playbacks.
		// ReadHeaderTimeout still guards against a client that stalls mid
		// request line.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return serve(server, log, opts)
}

func serve(server *http.Server, log *slog.Logger, opts options) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}

	log.Info("serving read-only WebDAV",
		"address", listener.Addr().String(),
		"auth", opts.username != "" || opts.password != "")
	for _, hint := range mountHints(listener.Addr(), opts) {
		log.Info(hint)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errs := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	// Give in-flight range requests a moment; players reconnect anyway.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Warn("shutdown was not clean", "err", err)
	}
	return nil
}

// openStatic builds a client from a cookie given on the command line, in a
// file, or in the environment.
//
// The cookie is checked against 115 before serving starts: it cannot be
// replaced without a restart, so a bad one is worth failing on immediately.
func openStatic(opts options, build func(string) (*pan115.Client, error), log *slog.Logger) (dav.Backend, error) {
	cookie, err := readCookie(opts)
	if err != nil {
		return nil, err
	}
	client, err := build(cookie)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.CheckAccess(ctx); err != nil {
		return nil, fmt.Errorf("cannot reach 115: %w", err)
	}
	log.Info("115 session accepted", "root", opts.root)
	return client, nil
}

// openSynced builds a backend that reads its cookie from a cookie-sync server
// and re-reads it whenever 115 stops accepting it.
//
// Unlike the static mode, the cookie is not checked against 115 here. A stale
// one on the server is the ordinary state this mode exists to recover from,
// and it costs nothing to let the first real request discover it.
func openSynced(opts options, build func(string) (*pan115.Client, error), log *slog.Logger) (*session.Session, error) {
	key, err := readCookieKey(opts)
	if err != nil {
		return nil, err
	}
	source, err := cookiesync.New(cookiesync.Config{
		Server:  opts.cookieServer,
		Channel: opts.cookieChannel,
		Key:     key,
		Domain:  opts.cookieDomain,
	})
	if err != nil {
		return nil, err
	}
	if source.Insecure() {
		log.Warn("the cookie server is plain HTTP, so the channel key and the 115 cookies both cross the network in the clear",
			"server", source.Server())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read once now, so that a wrong channel name, key or domain is reported
	// at startup rather than at the first PROPFIND.
	cookie, err := source.Fetch(ctx)
	switch {
	case errors.Is(err, cookiesync.ErrRejected):
		return nil, err
	case errors.Is(err, cookiesync.ErrNoDomain):
		if names, listErr := source.Domains(ctx); listErr == nil && len(names) > 0 {
			return nil, fmt.Errorf("%w; this channel holds: %s", err, strings.Join(names, ", "))
		}
		return nil, err
	case err != nil:
		// Transient, and the router may simply not be online yet. Carry on
		// with nothing; the first request will fetch.
		log.Warn("could not read the cookie server at startup, will try again on the first request", "err", err)
		cookie = ""
	default:
		log.Info("read cookies from the cookie server",
			"server", source.Server(), "channel", opts.cookieChannel, "domain", opts.cookieDomain)
	}

	if cookie != "" {
		if _, err := build(cookie); err != nil {
			// An incomplete set from the browser extension. The next upload
			// fixes it, so this is not worth refusing to start over.
			log.Warn("the cookies on the server are not usable yet", "err", err)
			cookie = ""
		}
	}

	return session.New(session.Options{
		Source:   source,
		Build:    build,
		Cookie:   cookie,
		Blackout: opts.cookieRetry,
		Logger:   log,
	})
}

// readCookieKey takes the channel key from whichever source was configured.
func readCookieKey(opts options) (string, error) {
	if opts.cookieKeyFile != "" {
		raw, err := os.ReadFile(opts.cookieKeyFile)
		if err != nil {
			return "", fmt.Errorf("read cookie key file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if opts.cookieKey != "" {
		return opts.cookieKey, nil
	}
	if env := os.Getenv("PAN115_COOKIE_KEY"); env != "" {
		return env, nil
	}
	return "", errors.New("no channel key given: pass -cookie-key, -cookie-key-file, or set PAN115_COOKIE_KEY")
}

func mountHints(addr net.Addr, opts options) []string {
	host := "127.0.0.1"
	if tcp, ok := addr.(*net.TCPAddr); ok {
		if tcp.IP != nil && !tcp.IP.IsUnspecified() {
			host = tcp.IP.String()
		}
		host = net.JoinHostPort(host, fmt.Sprint(tcp.Port))
	}
	hints := []string{"mount this URL in Infuse: http://" + host + "/"}
	if opts.username == "" && opts.password == "" {
		hints = append(hints, "no password is set; anyone who can reach this port can read the account")
	}
	return hints
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.listen, "listen", ":8115", "address to listen on")
	flag.StringVar(&opts.cookie, "cookie", "", "115 cookie (UID=..; CID=..; SEID=..; KID=..), or set PAN115_COOKIE")
	flag.StringVar(&opts.cookieFile, "cookie-file", "", "read the cookie from this file instead")
	flag.StringVar(&opts.root, "root", pan115.RootID, "115 directory id to mount")
	flag.StringVar(&opts.username, "user", os.Getenv("PAN115_USER"), "username for HTTP basic auth (optional)")
	flag.StringVar(&opts.password, "pass", os.Getenv("PAN115_PASS"), "password for HTTP basic auth (optional)")
	flag.DurationVar(&opts.dirTTL, "dir-ttl", 5*time.Minute, "how long to cache directory listings")
	flag.DurationVar(&opts.linkTTL, "link-ttl", 2*time.Hour, "how long to reuse a download URL before resolving again")
	flag.Float64Var(&opts.rate, "rate", pan115.DefaultRate, "requests per second allowed against the 115 API")
	flag.IntVar(&opts.pageSize, "page-size", pan115.DefaultPageSize, "directory listing page size (max 1150)")
	flag.StringVar(&opts.userAgent, "ua", pan115.DefaultUserAgent, "User-Agent to present to 115")
	flag.BoolVar(&opts.verbose, "v", false, "log every request")
	flag.BoolVar(&opts.showVersion, "version", false, "print the version and exit")

	flag.StringVar(&opts.cookieServer, "cookie-server", os.Getenv("PAN115_COOKIE_SERVER"),
		"read the cookie from a cookie-sync server instead, e.g. https://sync.example.com")
	flag.StringVar(&opts.cookieChannel, "cookie-channel", os.Getenv("PAN115_COOKIE_CHANNEL"), "channel name on that server")
	flag.StringVar(&opts.cookieKey, "cookie-key", "", "channel key, or set PAN115_COOKIE_KEY")
	flag.StringVar(&opts.cookieKeyFile, "cookie-key-file", "", "read the channel key from this file instead")
	flag.StringVar(&opts.cookieDomain, "cookie-domain", cmpOr(os.Getenv("PAN115_COOKIE_DOMAIN"), "115.com"),
		"which domain to read inside the channel")
	flag.DurationVar(&opts.cookieRetry, "cookie-retry", session.DefaultBlackout,
		"how long to answer 503 after the cookie server fails to supply working cookies")

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "115dav %s serves a 115 account as a read-only WebDAV mount.\n\n", version)
		fmt.Fprintf(out, "usage: %s -cookie 'UID=..; CID=..; SEID=..' [flags]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	return opts
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// checkModes rejects a configuration that names both cookie sources. They
// behave differently on failure, so silently preferring one would make the
// other's semantics a surprise.
func checkModes(opts options) error {
	if opts.cookieServer == "" {
		return nil
	}
	var given []string
	if opts.cookie != "" {
		given = append(given, "-cookie")
	}
	if opts.cookieFile != "" {
		given = append(given, "-cookie-file")
	}
	if os.Getenv("PAN115_COOKIE") != "" {
		given = append(given, "PAN115_COOKIE")
	}
	if len(given) > 0 {
		return fmt.Errorf("-cookie-server cannot be combined with %s: choose one source for the cookie",
			strings.Join(given, " or "))
	}
	if opts.cookieChannel == "" {
		return errors.New("-cookie-server needs -cookie-channel")
	}
	return nil
}

// readCookie takes the cookie from whichever source was configured, preferring
// the file so that a long secret need not sit in the process arguments.
func readCookie(opts options) (string, error) {
	if opts.cookieFile != "" {
		raw, err := os.ReadFile(opts.cookieFile)
		if err != nil {
			return "", fmt.Errorf("read cookie file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if opts.cookie != "" {
		return opts.cookie, nil
	}
	if env := os.Getenv("PAN115_COOKIE"); env != "" {
		return env, nil
	}
	return "", errors.New("no cookie given: pass -cookie, -cookie-file, or set PAN115_COOKIE")
}
