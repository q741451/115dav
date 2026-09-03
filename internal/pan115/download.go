package pan115

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Target is a resolved, ready-to-fetch location for one file.
//
// Header is not optional decoration: the CDN checks the User-Agent and session
// cookies against the ones that asked for the URL, so it must be replayed
// verbatim on the request that actually reads the bytes.
type Target struct {
	URL    string
	Name   string
	Size   int64
	Header http.Header
}

// ErrNotDownloadable reports that 115 acknowledged the pick code but declined
// to hand out a URL for it.
type ErrNotDownloadable struct{ PickCode string }

func (e *ErrNotDownloadable) Error() string {
	return fmt.Sprintf("115 returned no download URL for pick code %s", e.PickCode)
}

// Resolve turns a file's pick code into a CDN URL.
//
// The URL is short-lived and bound to the identity in Target.Header. Callers
// should be prepared to resolve again when the CDN starts refusing it.
func (c *Client) Resolve(ctx context.Context, pickCode string) (*Target, error) {
	if pickCode == "" {
		return nil, fmt.Errorf("115: empty pick code")
	}

	key, err := newSessionKey()
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]string{"pickcode": pickCode})
	if err != nil {
		return nil, err
	}
	sealed, err := sealRequest(request, key)
	if err != nil {
		return nil, fmt.Errorf("seal download request: %w", err)
	}

	var envelope downloadResponse
	resp, err := c.postForm(ctx,
		c.downloadURL,
		url.Values{"t": {strconv.FormatInt(time.Now().Unix(), 10)}},
		url.Values{"data": {sealed}},
		&envelope,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", pickCode, err)
	}

	plaintext, err := openResponse(string(envelope.Data), key)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", pickCode, err)
	}

	// The payload is keyed by file id, with a single entry in practice.
	var files map[string]downloadEntry
	if err := json.Unmarshal(plaintext, &files); err != nil {
		return nil, fmt.Errorf("resolve %s: decode payload: %w", pickCode, err)
	}

	for _, file := range files {
		if file.URL.Value == "" {
			continue
		}
		return &Target{
			URL:    file.URL.Value,
			Name:   file.Name,
			Size:   int64(file.Size),
			Header: c.streamHeader(resp.Cookies()),
		}, nil
	}
	return nil, &ErrNotDownloadable{PickCode: pickCode}
}

// streamHeader builds the identity to replay against the CDN: the same
// User-Agent and cookies that were used to ask for the URL, plus anything the
// download endpoint handed back.
func (c *Client) streamHeader(extra []*http.Cookie) http.Header {
	h := http.Header{}
	c.decorate(h)

	if len(extra) == 0 {
		return h
	}
	cookies := []string{c.cookie}
	for _, cookie := range extra {
		if cookie != nil {
			cookies = append(cookies, cookie.Name+"="+cookie.Value)
		}
	}
	h.Set("Cookie", strings.Join(cookies, "; "))
	return h
}

type downloadResponse struct {
	envelope
	// Data is the sealed payload; see crypto.go.
	Data flexString `json:"data"`
}

func (r *downloadResponse) status() envelope { return r.envelope }

type downloadEntry struct {
	Name string      `json:"file_name"`
	Size flexInt64   `json:"file_size"`
	URL  downloadURL `json:"url"`
}

// downloadURL is an object when a URL exists and the literal false when the
// file cannot be served, so it needs a tolerant decoder.
type downloadURL struct {
	Value string
}

func (u *downloadURL) UnmarshalJSON(b []byte) error {
	switch s := string(b); s {
	case "", "false", "null", `""`:
		return nil
	}
	var object struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(b, &object); err != nil {
		return fmt.Errorf("expected a url object or false, got %s", snippet(b))
	}
	u.Value = object.URL
	return nil
}
