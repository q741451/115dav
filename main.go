// Command pan115dav serves a 115 account as a read-only WebDAV mount, so that
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
	"strings"
	"syscall"
	"time"

	"github.com/q741451/115dav/internal/dav"
	"github.com/q741451/115dav/internal/pan115"
)

type options struct {
	listen     string
	cookie     string
	cookieFile string
	root       string
	username   string
	password   string
	dirTTL     time.Duration
	linkTTL    time.Duration
	rate       float64
	pageSize   int
	userAgent  string
	verbose    bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pan115dav:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := parseFlags()

	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cookie, err := readCookie(opts)
	if err != nil {
		return err
	}

	client, err := pan115.New(pan115.Config{
		Cookie:            cookie,
		UserAgent:         opts.userAgent,
		RequestsPerSecond: opts.rate,
		PageSize:          opts.pageSize,
	})
	if err != nil {
		return err
	}

	// Fail loudly at startup rather than at the first PROPFIND: a bad cookie
	// is the overwhelmingly common setup mistake.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.CheckAccess(ctx); err != nil {
		return fmt.Errorf("cannot reach 115: %w", err)
	}
	log.Info("115 session accepted", "root", opts.root)

	handler := dav.New(dav.Options{
		Backend:  client,
		RootID:   opts.root,
		DirTTL:   opts.dirTTL,
		LinkTTL:  opts.linkTTL,
		Username: opts.username,
		Password: opts.password,
		Logger:   log,
	})

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

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "pan115dav serves a 115 account as a read-only WebDAV mount.\n\n")
		fmt.Fprintf(out, "usage: %s -cookie 'UID=..; CID=..; SEID=..' [flags]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	return opts
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
