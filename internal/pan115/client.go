// Package pan115 is a read-only client for the 115 web API.
//
// It covers exactly what serving files over WebDAV needs: listing a directory
// and turning a pick code into a CDN URL that can be fetched. Nothing here
// writes to the account.
package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	apiFileList = "https://webapi.115.com/files"
	apiDownload = "https://proapi.115.com/app/chrome/downurl"

	// DefaultUserAgent identifies us as the desktop client. The CDN ties each
	// download URL to the User-Agent that asked for it, so whatever value is
	// used here must also be replayed when fetching the file itself.
	DefaultUserAgent = "Mozilla/5.0 115Browser/27.0.5.7"

	// maxPageSize is the largest page the listing endpoint will serve.
	maxPageSize = 1150

	// maxAPIBody caps how much of an API response we will read. Listings of a
	// full page run to a few hundred KB; anything past this is a captive
	// portal or an error page, not JSON we want to buffer.
	maxAPIBody = 32 << 20
)

// Config describes how to talk to 115.
type Config struct {
	// Cookie is the browser cookie for the account. All four of UID, CID,
	// SEID and KID must be present; anything else pasted alongside them is
	// passed through untouched.
	Cookie string

	// UserAgent defaults to DefaultUserAgent.
	UserAgent string

	// RequestsPerSecond throttles calls to the 115 API. It does not apply to
	// reading file bytes from the CDN. Zero means DefaultRate.
	RequestsPerSecond float64

	// PageSize is the directory listing page size. Zero means DefaultPageSize.
	PageSize int

	// Timeout bounds a single API call. It is deliberately not applied to
	// media transfers, which may legitimately run for hours.
	Timeout time.Duration
}

// Defaults for the zero values in Config.
const (
	DefaultRate     = 2.0
	DefaultPageSize = 1000
	DefaultTimeout  = 30 * time.Second
)

// Client is a read-only 115 API client. It is safe for concurrent use.
type Client struct {
	http      *http.Client
	limiter   *rate.Limiter
	cookie    string
	userAgent string
	pageSize  int

	// Endpoints are fields rather than constants so tests can point the
	// client at a stub. They are never changed at run time.
	listURL     string
	downloadURL string
}

// New validates the configuration and returns a client. It performs no I/O;
// call CheckAccess to confirm the cookie actually works.
func New(cfg Config) (*Client, error) {
	cookie, err := normaliseCookie(cfg.Cookie)
	if err != nil {
		return nil, err
	}

	pageSize := cmpOr(cfg.PageSize, DefaultPageSize)
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	return &Client{
		http:        &http.Client{Timeout: cmpOr(cfg.Timeout, DefaultTimeout)},
		limiter:     rate.NewLimiter(rate.Limit(cmpOr(cfg.RequestsPerSecond, DefaultRate)), 1),
		cookie:      cookie,
		userAgent:   cmpOr(cfg.UserAgent, DefaultUserAgent),
		pageSize:    pageSize,
		listURL:     apiFileList,
		downloadURL: apiDownload,
	}, nil
}

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// requiredCookies must all be present for the API to accept us. KID is not
// optional despite looking like a newer addition: without it the file list
// endpoint answers 990001, "login timed out", as if the session were stale.
var requiredCookies = []string{"UID", "CID", "SEID", "KID"}

// normaliseCookie checks that a pasted cookie carries a usable 115 session and
// tidies it into a header value.
//
// Everything pasted is forwarded, not just the names above: that is what the
// browser does, and a cookie 115 starts requiring later then works without a
// change here. Only the required names are normalised, so a cookie copied with
// lowercase names still matches.
func normaliseCookie(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"Cookie:", "cookie:"} {
		raw = strings.TrimPrefix(raw, prefix)
	}

	var (
		pairs   []string
		present = map[string]bool{}
	)
	for _, pair := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	}) {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		if upper := strings.ToUpper(name); slices.Contains(requiredCookies, upper) {
			name = upper
			present[upper] = true
		}
		pairs = append(pairs, name+"="+value)
	}

	var missing []string
	for _, name := range requiredCookies {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("cookie is missing %s; copy every 115.com cookie from your browser, not just some of them",
			strings.Join(missing, ", "))
	}
	return strings.Join(pairs, "; "), nil
}

// APIError is a structured failure reported by the 115 API itself, as opposed
// to a transport error.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("115 api error %d", e.Code)
	}
	return fmt.Sprintf("115 api error %d: %s", e.Code, e.Message)
}

// ErrNotAuthorized reports that 115 rejected the cookie. Callers use it to
// tell a stale login apart from a transient failure.
var ErrNotAuthorized = errors.New("115 rejected the cookie (expired or invalid)")

// envelope is the status preamble shared by the JSON endpoints.
type envelope struct {
	State   flexBool `json:"state"`
	Errno   flexInt  `json:"errno"`
	ErrNo   flexInt  `json:"errNo"`
	Code    flexInt  `json:"code"`
	Error   string   `json:"error"`
	Message string   `json:"msg"`
}

func (e envelope) err() error {
	if bool(e.State) {
		return nil
	}
	code := firstNonZero(int(e.Errno), int(e.ErrNo), int(e.Code))
	message := cmpOr(e.Error, e.Message)
	if isAuthCode(code) || mentionsLogin(message) {
		return fmt.Errorf("%w: %s", ErrNotAuthorized, cmpOr(message, "not logged in"))
	}
	return &APIError{Code: code, Message: message}
}

// Codes 115 uses for an unusable session.
func isAuthCode(code int) bool {
	switch code {
	case 99, 990001, 40101004, 40101017:
		return true
	}
	return false
}

func mentionsLogin(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "not login") || strings.Contains(m, "登录")
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// get issues a rate-limited GET and decodes the JSON body into out, which must
// embed envelope so the API-level status can be checked.
func (c *Client) get(ctx context.Context, endpoint string, query url.Values, out interface{ status() envelope }) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	_, err = c.do(req, out)
	return err
}

// postForm issues a rate-limited form POST. It returns the response so callers
// can inspect headers; the body has already been consumed.
func (c *Client) postForm(ctx context.Context, endpoint string, query, form url.Values, out interface{ status() envelope }) (*http.Response, error) {
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out interface{ status() envelope }) (*http.Response, error) {
	if err := c.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	c.decorate(req.Header)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", req.URL.Host, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %s", req.URL.Host, resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w (body starts %q)", req.URL.Host, err, snippet(body))
	}
	if err := out.status().err(); err != nil {
		return nil, err
	}
	return resp, nil
}

// decorate applies the identity every 115 request needs.
func (c *Client) decorate(h http.Header) {
	h.Set("User-Agent", c.userAgent)
	h.Set("Cookie", c.cookie)
}

func snippet(b []byte) string {
	const limit = 120
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}

// Flexible scalars: 115 is inconsistent about quoting numbers and booleans,
// and the same field can change shape between endpoints.

type flexInt int

func (v *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("expected an integer, got %s", b)
	}
	*v = flexInt(n)
	return nil
}

type flexInt64 int64

func (v *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	// Sizes occasionally arrive in scientific notation.
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %s", b)
	}
	*v = flexInt64(n)
	return nil
}

type flexString string

func (v *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return err
		}
		*v = flexString(unquoted)
		return nil
	}
	*v = flexString(s)
	return nil
}

type flexBool bool

func (v *flexBool) UnmarshalJSON(b []byte) error {
	switch s := strings.Trim(string(b), `"`); s {
	case "true", "1":
		*v = true
	case "false", "0", "", "null":
		*v = false
	default:
		return fmt.Errorf("expected a boolean, got %s", b)
	}
	return nil
}
