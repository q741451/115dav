package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// newTestClient points a client at a stub that speaks the 115 wire format.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := New(Config{
		Cookie:            "UID=u; CID=c; SEID=s; KID=k",
		RequestsPerSecond: 1000, // the limiter is not under test
	})
	if err != nil {
		t.Fatal(err)
	}
	client.listURL = server.URL + "/files"
	client.downloadURL = server.URL + "/downurl"
	return client, server
}

// listStub serves a directory in pages, mimicking the abbreviated field names
// and quoted numbers the real endpoint uses.
func listStub(t *testing.T, cid string, total int, pageLimit int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit > pageLimit {
			limit = pageLimit
		}

		items := []map[string]any{}
		for i := offset; i < total && len(items) < limit; i++ {
			if i == 0 {
				// A directory: no "fid", and "t" is a Unix timestamp.
				items = append(items, map[string]any{
					"cid": "9001", "pid": cid, "n": "Season 1", "t": "1700000000",
					"s": "0", "pc": "",
				})
				continue
			}
			items = append(items, map[string]any{
				"fid": fmt.Sprintf("f%d", i), "cid": cid,
				"n": fmt.Sprintf("ep%02d.mkv", i),
				// Sizes and timestamps arrive quoted, wall-clock and zone-less.
				"s": strconv.Itoa(1000 + i), "t": "2024-03-01 21:30",
				"pc": fmt.Sprintf("pick%d", i), "sha": "deadbeef",
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": cid, "count": total, "offset": offset, "data": items,
		})
	}
}

func TestListPaginates(t *testing.T) {
	const total = 25
	client, _ := newTestClient(t, listStub(t, "0", total, 10))

	entries, err := client.List(context.Background(), "0")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("got %d entries, want %d", len(entries), total)
	}

	dir := entries[0]
	if !dir.IsDir || dir.ID != "9001" || dir.Name != "Season 1" {
		t.Errorf("first entry = %+v, want the directory Season 1 with id 9001", dir)
	}
	if want := time.Unix(1700000000, 0); !dir.ModTime.Equal(want) {
		t.Errorf("directory mtime = %s, want %s", dir.ModTime, want)
	}

	file := entries[1]
	if file.IsDir || file.ID != "f1" || file.Size != 1001 || file.PickCode != "pick1" {
		t.Errorf("second entry = %+v, want file f1", file)
	}
	if want := time.Date(2024, 3, 1, 21, 30, 0, 0, chinaStandardTime); !file.ModTime.Equal(want) {
		t.Errorf("file mtime = %s, want %s", file.ModTime, want)
	}
}

// 115 quietly serves the root when asked for a directory that does not exist,
// which would otherwise surface as the wrong listing.
func TestListRejectsSubstitutedDirectory(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "0", "count": 0, "offset": 0, "data": []any{},
		})
	})

	if _, err := client.List(context.Background(), "12345"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A directory the endpoint will not finish serving must be an error, not a
// short listing. Returning the entries that did arrive would hand a client a
// truncated directory that looks exactly like a complete small one -- and a
// media client responds to a library that shrank by discarding what it knew.
//
// Terminating at all is the other half of the requirement: a count that
// overstates what will be served must not page forever.
func TestListRefusesToTruncate(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 20 {
			t.Fatal("List kept paging past the end of the directory")
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		items := []map[string]any{}
		if offset == 0 {
			items = append(items, map[string]any{"fid": "f1", "cid": "0", "n": "only.mkv", "s": "1"})
		}
		// Claims far more entries than it is willing to serve.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "0", "count": 999, "offset": offset, "data": items,
		})
	})

	entries, err := client.List(context.Background(), "0")
	if err == nil {
		t.Fatalf("List returned %d of 999 entries and no error", len(entries))
	}
	if entries != nil {
		t.Errorf("List returned %d entries alongside an error; callers must get nothing to cache", len(entries))
	}
}

// An absent count is not a count of zero. Decoding it as one would end the
// walk after the first page, silently losing everything past it.
func TestListRefusesAPageWithoutACount(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "0", "offset": 0,
			"data": []map[string]any{{"fid": "f1", "cid": "0", "n": "one.mkv", "s": "1"}},
		})
	})

	if _, err := client.List(context.Background(), "0"); err == nil {
		t.Fatal("List accepted a page with no count")
	}
}

// The shape of a directory 115 has decided not to serve: a count that says
// there is something there, and nothing in the payload. Reporting it as empty
// is how a live library turns into a deleted one.
func TestListRefusesAnEmptyPageBelowTheCount(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "0", "count": 67, "offset": 0, "data": []any{},
		})
	})

	if _, err := client.List(context.Background(), "0"); err == nil {
		t.Fatal("List reported a 67-entry directory as empty")
	}
}

// A genuinely empty directory still lists, and still costs one call.
func TestListAcceptsAnEmptyDirectory(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "7", "count": 0, "offset": 0, "data": []any{},
		})
	})

	entries, err := client.List(context.Background(), "7")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 || calls != 1 {
		t.Errorf("got %d entries in %d calls, want 0 in 1", len(entries), calls)
	}
}

// A directory that shrinks between pages is not a contradiction: the count
// drops with it, and what was already collected is still worth serving.
func TestListToleratesADirectoryThatShrinks(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		items := []map[string]any{}
		count := 1500
		if offset == 0 {
			for i := range 1000 {
				items = append(items, map[string]any{
					"fid": fmt.Sprintf("f%d", i), "cid": "0",
					"n": fmt.Sprintf("ep%d.mkv", i), "s": "1",
				})
			}
		} else {
			count = 800 // half the directory was deleted meanwhile
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": true, "cid": "0", "count": count, "offset": offset, "data": items,
		})
	})

	entries, err := client.List(context.Background(), "0")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1000 {
		t.Errorf("got %d entries, want the 1000 that were served", len(entries))
	}
}

func TestListSurfacesAPIErrors(t *testing.T) {
	for name, payload := range map[string]struct {
		body    map[string]any
		wantErr error
	}{
		"expired session": {
			body:    map[string]any{"state": false, "errno": 99, "error": "not login"},
			wantErr: ErrNotAuthorized,
		},
		"other failure": {
			body:    map[string]any{"state": false, "errno": "20004", "error": "busy"},
			wantErr: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(payload.body)
			})

			_, err := client.List(context.Background(), "0")
			if err == nil {
				t.Fatal("want an error")
			}
			if payload.wantErr != nil && !errors.Is(err, payload.wantErr) {
				t.Fatalf("err = %v, want %v", err, payload.wantErr)
			}
			var apiErr *APIError
			if payload.wantErr == nil && !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want an *APIError", err)
			}
		})
	}
}

func TestParseModTime(t *testing.T) {
	for raw, want := range map[string]time.Time{
		"1700000000":          time.Unix(1700000000, 0),
		"2024-03-01 21:30":    time.Date(2024, 3, 1, 21, 30, 0, 0, chinaStandardTime),
		"2024-03-01 21:30:45": time.Date(2024, 3, 1, 21, 30, 45, 0, chinaStandardTime),
		"":                    {},
		"not a time":          {},
	} {
		if got := parseModTime(raw); !got.Equal(want) {
			t.Errorf("parseModTime(%q) = %s, want %s", raw, got, want)
		}
	}
}
