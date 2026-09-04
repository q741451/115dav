package cookiesync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stub speaks the parts of the cookie-sync wire format this package reads.
func stub(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := New(Config{
		Server:  server.URL,
		Channel: "test",
		Key:     "secret",
		Domain:  "115.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestFetch(t *testing.T) {
	var gotName, gotKey, gotDomain string
	client := stub(t, func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get("X-Channel-Name")
		gotKey = r.Header.Get("X-Channel-Key")
		gotDomain = r.URL.Query().Get("domain")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "domain": "115.com", "updated_at": 1788503532,
			"cookies": []map[string]any{
				{"name": "UID", "value": "u", "domain": ".115.com", "httpOnly": true},
				{"name": "CID", "value": "c", "domain": ".115.com"},
				{"name": "SEID", "value": "s", "domain": ".115.com"},
				{"name": "KID", "value": "k", "domain": ".115.com"},
				{"name": "acw_tc", "value": "a", "domain": "115.com"},
			},
		})
	})

	got, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := "UID=u; CID=c; SEID=s; KID=k; acw_tc=a"; got != want {
		t.Errorf("cookie = %q, want %q", got, want)
	}
	if gotName != "test" || gotKey != "secret" {
		t.Errorf("authenticated as %q/%q, want test/secret", gotName, gotKey)
	}
	if gotDomain != "115.com" {
		t.Errorf("asked for domain %q, want 115.com", gotDomain)
	}
}

// Cookies with no name or no value would produce a malformed header.
func TestFetchSkipsEmptyCookies(t *testing.T) {
	client := stub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"cookies": []map[string]any{
				{"name": "UID", "value": "u"},
				{"name": "", "value": "orphan"},
				{"name": "empty", "value": ""},
			},
		})
	})

	got, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := "UID=u"; got != want {
		t.Errorf("cookie = %q, want %q", got, want)
	}
}

// A wrong channel name or key will not fix itself, so it is told apart from a
// server that is merely unreachable.
func TestFetchErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		body    map[string]any
		wantErr error
	}{
		"wrong key": {
			status:  http.StatusUnauthorized,
			body:    map[string]any{"error": "invalid channel name or key"},
			wantErr: ErrRejected,
		},
		"read-only key used for a write": {
			status:  http.StatusForbidden,
			body:    map[string]any{"error": "this key is read-only and cannot upload"},
			wantErr: ErrRejected,
		},
		"unknown domain": {
			status:  http.StatusNotFound,
			body:    map[string]any{"error": "no data found for this domain in this channel"},
			wantErr: ErrNoDomain,
		},
		"server broken": {
			status:  http.StatusInternalServerError,
			body:    map[string]any{"error": "internal error"},
			wantErr: nil, // transient: no sentinel, so callers retry
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := stub(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			})

			_, err := client.Fetch(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (errors.Is(err, ErrRejected) || errors.Is(err, ErrNoDomain)) {
				t.Fatalf("err = %v, want a retryable error", err)
			}
			// The server's own words help far more than a generic message.
			if want := tc.body["error"].(string); !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, does not carry the server's message %q", err, want)
			}
		})
	}
}

// An entry that exists but holds nothing usable is the same problem as no
// entry at all.
func TestFetchEmptyEntry(t *testing.T) {
	client := stub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "cookies": []any{}})
	})

	if _, err := client.Fetch(context.Background()); !errors.Is(err, ErrNoDomain) {
		t.Fatalf("err = %v, want ErrNoDomain", err)
	}
}

func TestDomains(t *testing.T) {
	client := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/list_domains" {
			t.Errorf("asked for %q, want /api/list_domains", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"domains": []map[string]any{
				{"domain": "115.com", "updated_at": 1},
				{"domain": "example.com", "updated_at": 2},
			},
		})
	})

	got, err := client.Domains(context.Background())
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(got) != 2 || got[0] != "115.com" || got[1] != "example.com" {
		t.Errorf("domains = %q, want [115.com example.com]", got)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no server":    {Channel: "c", Key: "k", Domain: "d"},
		"not a url":    {Server: "://nope", Channel: "c", Key: "k", Domain: "d"},
		"wrong scheme": {Server: "ftp://host", Channel: "c", Key: "k", Domain: "d"},
		"no host":      {Server: "http://", Channel: "c", Key: "k", Domain: "d"},
		"no channel":   {Server: "http://h", Key: "k", Domain: "d"},
		"no key":       {Server: "http://h", Channel: "c", Domain: "d"},
		"no domain":    {Server: "http://h", Channel: "c", Key: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestInsecureReportsPlainHTTP(t *testing.T) {
	for server, want := range map[string]bool{
		"http://host:2500":  true,
		"https://host:2500": false,
	} {
		client, err := New(Config{Server: server, Channel: "c", Key: "k", Domain: "d"})
		if err != nil {
			t.Fatal(err)
		}
		if got := client.Insecure(); got != want {
			t.Errorf("Insecure(%s) = %v, want %v", server, got, want)
		}
	}
}

// A trailing slash on the server URL must not produce a double slash.
func TestServerURLIsNormalised(t *testing.T) {
	client, err := New(Config{Server: "http://host:2500/", Channel: "c", Key: "k", Domain: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Server(); got != "http://host:2500" {
		t.Errorf("Server() = %q, want http://host:2500", got)
	}
}
