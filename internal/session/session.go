// Package session keeps a 115 client's cookies current by re-reading them
// from a cookie-sync server whenever 115 stops accepting them.
//
// It exists so that an unattended process -- on a router, say -- survives a
// login expiring: the cookies are replaced remotely, and the next request
// picks them up. Switching is not seamless and does not try to be. A refresh
// throws away the old client and every cache built with it, because the
// channel may by then hold a different 115 account entirely, whose directory
// ids and pick codes have nothing to do with the ones already cached.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/q741451/115dav/internal/cookiesync"
	"github.com/q741451/115dav/internal/dav"
	"github.com/q741451/115dav/internal/pan115"
)

// Source supplies a cookie. It is *cookiesync.Client in production.
type Source interface {
	Fetch(ctx context.Context) (string, error)
}

// Options configures a Session.
type Options struct {
	// Source reads the current cookie from the cookie-sync server.
	Source Source

	// Build turns a cookie into a client. Failure means the cookie is
	// unusable -- incomplete, say -- which is treated the same as an expired
	// one: it will be fixed by the next upload, not by exiting.
	Build func(cookie string) (*pan115.Client, error)

	// Cookie, when set, is the cookie already fetched at startup, so that the
	// first request does not have to fetch again.
	Cookie string

	// Blackout is how long to stop trying after a refresh fails to produce
	// working cookies. During it neither 115 nor the cookie server is
	// contacted at all.
	Blackout time.Duration

	Logger *slog.Logger
}

// fetchTimeout bounds a cookie fetch that no longer has a caller waiting.
const fetchTimeout = 30 * time.Second

// DefaultBlackout is long enough that a person noticing the failure, logging
// in again and re-uploading is not racing a stream of retries.
const DefaultBlackout = 30 * time.Second

// Session is a dav.Backend that refreshes its own credentials.
type Session struct {
	opts   Options
	client atomic.Pointer[pan115.Client]
	cookie atomic.Pointer[string]

	group singleflight.Group

	// flush is wired after construction: it points at the server built on top
	// of this session, which cannot exist until this does.
	flush atomic.Pointer[func()]

	mu           sync.Mutex
	blockedUntil time.Time
}

var _ dav.Backend = (*Session)(nil)

// New returns a Session. A non-empty Options.Cookie is adopted immediately;
// otherwise the first request fetches one.
func New(opts Options) (*Session, error) {
	if opts.Source == nil || opts.Build == nil {
		return nil, errors.New("session needs a source and a way to build a client")
	}
	if opts.Blackout <= 0 {
		opts.Blackout = DefaultBlackout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	s := &Session{opts: opts}
	if opts.Cookie != "" {
		client, err := opts.Build(opts.Cookie)
		if err != nil {
			return nil, err
		}
		// Stored rather than adopted: at construction there is nothing built
		// on the old credentials, so there is nothing to discard.
		s.client.Store(client)
		s.cookie.Store(&opts.Cookie)
	}
	return s, nil
}

// OnRefresh registers what to discard when the credentials are replaced. It
// is set after construction because the caches in question belong to the
// server built on top of this session.
func (s *Session) OnRefresh(flush func()) {
	s.flush.Store(&flush)
}

func (s *Session) List(ctx context.Context, id string) ([]pan115.Entry, error) {
	return withRefresh(s, ctx, func(c *pan115.Client) ([]pan115.Entry, error) {
		return c.List(ctx, id)
	})
}

func (s *Session) Resolve(ctx context.Context, pickCode string) (*pan115.Target, error) {
	return withRefresh(s, ctx, func(c *pan115.Client) (*pan115.Target, error) {
		return c.Resolve(ctx, pickCode)
	})
}

// withRefresh runs op, and if 115 rejects the cookies, fetches once more and
// runs it again. A method cannot take a type parameter, hence the free
// function.
func withRefresh[T any](s *Session, ctx context.Context, op func(*pan115.Client) (T, error)) (T, error) {
	var zero T

	if s.blocked() {
		return zero, dav.ErrUnavailable
	}

	client := s.client.Load()
	if client == nil {
		// Nothing yet: the startup fetch failed, or there never was one.
		if err := s.refresh(ctx, ""); err != nil {
			return zero, err
		}
		if client = s.client.Load(); client == nil {
			// Belt and braces: a refresh that reports success has stored a
			// client, but this is a long-lived process on an unattended box
			// and a nil dereference here would be a poor way to find out
			// otherwise.
			return zero, dav.ErrUnavailable
		}
	}

	result, err := op(client)
	if !errors.Is(err, pan115.ErrNotAuthorized) {
		return result, err
	}

	// 115 no longer accepts what we have. Whatever the channel holds now is
	// the only thing that can help.
	s.opts.Logger.Info("115 rejected the current cookies, re-reading them from the cookie server")
	if err := s.refresh(ctx, s.currentCookie()); err != nil {
		return zero, err
	}

	result, err = op(s.client.Load())
	if errors.Is(err, pan115.ErrNotAuthorized) {
		s.block("the cookies on the server are rejected by 115 as well; update them from the browser")
		return zero, dav.ErrUnavailable
	}
	return result, err
}

// refresh reads the channel and adopts what it finds. stale is the cookie
// already known not to work; fetching it again would be pointless.
func (s *Session) refresh(ctx context.Context, stale string) error {
	_, err, _ := s.group.Do("refresh", func() (any, error) {
		// A request that queued behind another one's refresh may find the job
		// already done.
		if current := s.currentCookie(); current != "" && current != stale {
			return nil, nil
		}

		// Detached from the caller: the fetch is shared with everyone else
		// waiting on it, and the request that triggered it may hang up.
		fetch, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()

		cookie, err := s.opts.Source.Fetch(fetch)
		switch {
		case errors.Is(err, cookiesync.ErrRejected), errors.Is(err, cookiesync.ErrNoDomain):
			// The configuration is wrong and will not repair itself. Loud, but
			// not fatal: this process is expected to outlive such mistakes.
			s.opts.Logger.Error("cookie server will not serve this channel", "err", err)
			s.block("the cookie server rejected the channel or domain")
			return nil, dav.ErrUnavailable
		case err != nil:
			s.opts.Logger.Warn("cannot reach the cookie server", "err", err)
			s.block("the cookie server is unreachable")
			return nil, dav.ErrUnavailable
		}

		if cookie == stale {
			// Nobody has uploaded a new login yet. Trying it against 115 would
			// only earn another rejection.
			s.opts.Logger.Warn("the cookie server still holds the cookies 115 just rejected")
			s.block("waiting for new cookies to be uploaded")
			return nil, dav.ErrUnavailable
		}

		client, err := s.opts.Build(cookie)
		if err != nil {
			// Usually an incomplete set from the browser extension. The next
			// upload fixes it, so treat it like an expired login.
			s.opts.Logger.Warn("the cookies from the server are not usable", "err", err)
			s.block("the cookies on the server are incomplete")
			return nil, dav.ErrUnavailable
		}

		s.adopt(client, cookie)
		s.opts.Logger.Info("adopted new cookies from the cookie server")
		return nil, nil
	})
	return err
}

// adopt installs a client and drops everything built with the previous one.
func (s *Session) adopt(client *pan115.Client, cookie string) {
	s.client.Store(client)
	s.cookie.Store(&cookie)
	s.unblock()
	if flush := s.flush.Load(); flush != nil {
		(*flush)()
	}
}

func (s *Session) currentCookie() string {
	if cookie := s.cookie.Load(); cookie != nil {
		return *cookie
	}
	return ""
}

// block starts a quiet period. Requests during it are refused without
// touching either 115 or the cookie server.
func (s *Session) block(reason string) {
	s.mu.Lock()
	first := time.Now().After(s.blockedUntil)
	s.blockedUntil = time.Now().Add(s.opts.Blackout)
	s.mu.Unlock()

	if first {
		s.opts.Logger.Warn("serving 503 until the cookies work again",
			"reason", reason, "for", s.opts.Blackout)
	}
}

func (s *Session) unblock() {
	s.mu.Lock()
	s.blockedUntil = time.Time{}
	s.mu.Unlock()
}

func (s *Session) blocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.blockedUntil)
}

// Blocked reports whether requests are currently being refused, so the server
// can answer without entering the WebDAV machinery at all.
func (s *Session) Blocked() bool { return s.blocked() }
