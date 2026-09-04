// Package cookiesync reads a browser cookie set from a cookie-sync server.
//
// The server stores cookies per "channel" and, within a channel, per domain;
// see https://github.com/q741451/cookie-sync. This package only ever reads.
package cookiesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Failures worth telling apart: the first two mean the configuration is wrong
// and will not fix itself, while anything else is worth retrying later.
var (
	// ErrRejected means the server refused the channel name or key.
	ErrRejected = errors.New("cookie server rejected the channel name or key")

	// ErrNoDomain means the channel exists but holds nothing for the domain.
	ErrNoDomain = errors.New("no cookies stored for this domain in this channel")
)

// Config describes one channel on one server.
type Config struct {
	// Server is the base URL, e.g. "http://192.168.1.2:2500".
	Server string

	// Channel and Key authenticate against it. A read-only key is enough.
	Channel string
	Key     string

	// Domain selects one entry inside the channel. The browser extension
	// stores under the hostname of the tab it was invoked on, so this is
	// usually "115.com".
	Domain string

	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout is deliberately short: a fetch happens while a media client
// is waiting for an answer.
const DefaultTimeout = 15 * time.Second

// maxBody caps the response; a channel holds a handful of cookies.
const maxBody = 1 << 20

// Client reads one channel. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	base    string
	channel string
	key     string
	domain  string
}

// New validates the configuration and returns a client. It performs no I/O.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("cookie server must be an http or https URL, got %q", cfg.Server)
	}
	if cfg.Channel == "" || cfg.Key == "" {
		return nil, errors.New("cookie server needs both a channel name and a key")
	}
	if cfg.Domain == "" {
		return nil, errors.New("cookie server needs a domain to read")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	return &Client{
		http: &http.Client{
			Timeout: timeout,
			// Do not follow redirects. The channel key travels in a header of
			// our own, and net/http only strips the headers it knows are
			// sensitive when a redirect crosses hosts -- this one it would
			// happily hand to wherever the server pointed us.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				// No connection reuse. Fetches are rare -- once at startup and
				// then only when 115 rejects the current cookies -- and a
				// pooled connection would keep talking to an address resolved
				// long ago. A home server behind a dynamic address moves; this
				// way every fetch resolves the name again.
				DisableKeepAlives:   true,
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		base:    base,
		channel: cfg.Channel,
		key:     cfg.Key,
		domain:  cfg.Domain,
	}, nil
}

// Insecure reports whether the server is addressed over plain HTTP, in which
// case the channel key and the cookies both cross the network in the clear.
func (c *Client) Insecure() bool { return strings.HasPrefix(c.base, "http://") }

// Server returns the configured base URL, for logging.
func (c *Client) Server() string { return c.base }

// Fetch returns the stored cookies for the configured domain, formatted as a
// Cookie header value.
func (c *Client) Fetch(ctx context.Context) (string, error) {
	var body struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
		UpdatedAt int64 `json:"updated_at"`
	}
	if err := c.get(ctx, "/api/download?domain="+url.QueryEscape(c.domain), &body); err != nil {
		return "", err
	}

	pairs := make([]string, 0, len(body.Cookies))
	for _, cookie := range body.Cookies {
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("%w: the entry is empty", ErrNoDomain)
	}
	return strings.Join(pairs, "; "), nil
}

// Domains lists what the channel holds. It exists to turn "domain not found"
// into a message that says what the alternatives are.
func (c *Client) Domains(ctx context.Context) ([]string, error) {
	var body struct {
		Domains []struct {
			Domain string `json:"domain"`
		} `json:"domains"`
	}
	if err := c.get(ctx, "/api/list_domains", &body); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(body.Domains))
	for _, d := range body.Domains {
		names = append(names, d.Domain)
	}
	return names, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	// Built fresh each call rather than kept as a *url.URL, so that nothing
	// about a previous request survives into this one.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Channel-Name", c.channel)
	req.Header.Set("X-Channel-Key", c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach cookie server: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read from cookie server: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrRejected, serverMessage(payload))
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNoDomain, serverMessage(payload))
	default:
		return fmt.Errorf("cookie server returned HTTP %s: %s", resp.Status, serverMessage(payload))
	}

	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode cookie server response: %w", err)
	}
	return nil
}

// serverMessage pulls the error text out of a JSON reply, falling back to the
// raw body when it is not the shape we expect.
func serverMessage(payload []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && body.Error != "" {
		return body.Error
	}
	const limit = 120
	text := strings.TrimSpace(string(payload))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
