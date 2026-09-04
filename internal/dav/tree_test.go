package dav

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// stubBackend serves fixed listings and counts calls. It never resolves.
type stubBackend struct {
	dirs  map[string][]pan115.Entry
	calls atomic.Int64
	delay time.Duration
}

func (s *stubBackend) List(_ context.Context, id string) ([]pan115.Entry, error) {
	s.calls.Add(1)
	time.Sleep(s.delay)
	entries, ok := s.dirs[id]
	if !ok {
		return nil, pan115.ErrNotFound
	}
	// Fresh each call; the tree renames in place. See Backend.List.
	return append([]pan115.Entry(nil), entries...), nil
}

func (s *stubBackend) Resolve(context.Context, string) (*pan115.Target, error) {
	return nil, errors.New("not used")
}

func TestSplitPath(t *testing.T) {
	for path, want := range map[string][]string{
		"/":          nil,
		"":           nil,
		"/a":         {"a"},
		"a/b":        {"a", "b"},
		"/a/b/":      {"a", "b"},
		"/a//b":      {"a", "b"},
		"/a/./b":     {"a", "b"},
		"/a/b/../c":  {"a", "c"},
		"/中文/片子.mkv": {"中文", "片子.mkv"},
	} {
		if got := splitPath(path); !reflect.DeepEqual(got, want) {
			t.Errorf("splitPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSanitiseName(t *testing.T) {
	for name, want := range map[string]string{
		"ordinary.mkv":  "ordinary.mkv",
		"a/b.mkv":       "a_b.mkv",
		"a\\b.mkv":      "a_b.mkv",
		"tab\there.mkv": "tab_here.mkv",
		".":             "_.",
		"..":            "_..",
		"":              "_",
	} {
		if got := sanitiseName(name); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", name, got, want)
		}
	}
}

// Two children with the same name would otherwise make one of them
// unreachable, since paths are resolved by name.
func TestDuplicateNamesAreDisambiguated(t *testing.T) {
	dir := newDirectory([]pan115.Entry{
		{ID: "1", Name: "film.mkv", PickCode: "a"},
		{ID: "2", Name: "film.mkv", PickCode: "b"},
		{ID: "3", Name: "film.mkv", PickCode: "c"},
		{ID: "4", Name: "notes", PickCode: "d"},
		{ID: "5", Name: "notes", PickCode: "e"},
	}, time.Now().Add(time.Minute))

	var names []string
	for _, entry := range dir.entries {
		names = append(names, entry.Name)
	}
	want := []string{"film.mkv", "film (2).mkv", "film (3).mkv", "notes", "notes (2)"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}

	// Every entry must still be reachable by its displayed name.
	for _, entry := range dir.entries {
		i, ok := dir.byName[entry.Name]
		if !ok {
			t.Errorf("%q is not resolvable", entry.Name)
			continue
		}
		if found := dir.entries[i]; found.ID != entry.ID {
			t.Errorf("%q resolves to id %s, want %s", entry.Name, found.ID, entry.ID)
		}
	}
}

func TestLookupWalksAndCaches(t *testing.T) {
	backend := &stubBackend{dirs: map[string][]pan115.Entry{
		"0":  {{ID: "d1", Name: "Shows", IsDir: true}},
		"d1": {{ID: "d2", Name: "S1", IsDir: true}},
		"d2": {{ID: "f1", Name: "ep1.mkv", PickCode: "pc1", Size: 42}},
	}}
	tree := NewTree(context.Background(), backend, "0", time.Minute)

	entry, err := tree.Lookup(context.Background(), "/Shows/S1/ep1.mkv")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry.ID != "f1" || entry.Size != 42 {
		t.Fatalf("entry = %+v, want f1", entry)
	}
	if got := backend.calls.Load(); got != 3 {
		t.Fatalf("list calls = %d, want 3", got)
	}

	if _, err := tree.Lookup(context.Background(), "/Shows/S1/ep1.mkv"); err != nil {
		t.Fatal(err)
	}
	if got := backend.calls.Load(); got != 3 {
		t.Errorf("list calls = %d after a cached lookup, want 3", got)
	}
}

func TestLookupMissing(t *testing.T) {
	backend := &stubBackend{dirs: map[string][]pan115.Entry{
		"0": {{ID: "f1", Name: "film.mkv", PickCode: "pc1"}},
	}}
	tree := NewTree(context.Background(), backend, "0", time.Minute)

	for _, path := range []string{
		"/nope.mkv",
		"/film.mkv/deeper.mkv", // descending through a file
	} {
		if _, err := tree.Lookup(context.Background(), path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lookup(%q) err = %v, want fs.ErrNotExist", path, err)
		}
	}
}

func TestExpiredListingIsRefetched(t *testing.T) {
	backend := &stubBackend{dirs: map[string][]pan115.Entry{
		"0": {{ID: "f1", Name: "film.mkv"}},
	}}
	tree := NewTree(context.Background(), backend, "0", time.Nanosecond)

	for range 3 {
		if _, err := tree.Children(context.Background(), "0"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := backend.calls.Load(); got != 3 {
		t.Errorf("list calls = %d, want 3 once the TTL has lapsed each time", got)
	}
}

// A media scanner opens many directories at once; concurrent misses on the
// same directory must collapse into a single upstream call.
func TestConcurrentMissesCollapse(t *testing.T) {
	backend := &stubBackend{
		dirs:  map[string][]pan115.Entry{"0": {{ID: "f1", Name: "film.mkv"}}},
		delay: 20 * time.Millisecond,
	}
	tree := NewTree(context.Background(), backend, "0", time.Minute)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tree.Children(context.Background(), "0"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := backend.calls.Load(); got != 1 {
		t.Errorf("list calls = %d, want 1", got)
	}
}

func TestForgetDropsCache(t *testing.T) {
	backend := &stubBackend{dirs: map[string][]pan115.Entry{"0": {{ID: "f1", Name: "a.mkv"}}}}
	tree := NewTree(context.Background(), backend, "0", time.Hour)

	if _, err := tree.Children(context.Background(), "0"); err != nil {
		t.Fatal(err)
	}
	tree.Forget("0")
	if _, err := tree.Children(context.Background(), "0"); err != nil {
		t.Fatal(err)
	}
	if got := backend.calls.Load(); got != 2 {
		t.Errorf("list calls = %d, want 2", got)
	}
}

// resolveAny hands out a link for any pick code, so the cache can be filled.
type resolveAny struct{ calls atomic.Int64 }

func (r *resolveAny) List(context.Context, string) ([]pan115.Entry, error) {
	return nil, pan115.ErrNotFound
}

func (r *resolveAny) Resolve(_ context.Context, pickCode string) (*pan115.Target, error) {
	r.calls.Add(1)
	return &pan115.Target{URL: "http://example.invalid/" + pickCode, Header: http.Header{}}, nil
}

// An expired link must leave the map, not merely stop being served. Nothing
// else removes one: forget() only runs when the CDN starts refusing a link, so
// without this a library walked once would be retained for the life of the
// process.
func TestExpiredLinkIsEvicted(t *testing.T) {
	cache := newLinkCache(context.Background(), &resolveAny{}, time.Nanosecond)

	if _, err := cache.get(context.Background(), "pc-1"); err != nil {
		t.Fatal(err)
	}
	if got := len(cache.items); got != 1 {
		t.Fatalf("cached %d links, want 1", got)
	}

	time.Sleep(time.Millisecond)
	if target := cache.lookup("pc-1"); target != nil {
		t.Error("an expired link was served")
	}
	if got := len(cache.items); got != 0 {
		t.Errorf("%d expired links left behind, want 0", got)
	}
}

func TestLinkCacheStaysBounded(t *testing.T) {
	backend := &resolveAny{}
	cache := newLinkCache(context.Background(), backend, time.Hour) // nothing expires on its own

	for i := range maxCachedLinks + 500 {
		if _, err := cache.get(context.Background(), fmt.Sprintf("pc-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(cache.items); got > maxCachedLinks {
		t.Errorf("cached %d links, want at most %d", got, maxCachedLinks)
	}
	if got := backend.calls.Load(); got != int64(maxCachedLinks+500) {
		t.Errorf("resolved %d times, want %d", got, maxCachedLinks+500)
	}
}

// The tree takes ownership of the slice List returns and renames it in place.
// Copying it into a second slice first doubled the cost of the one thing that
// scales with the size of a directory -- for a 50000-entry directory, several
// megabytes of pure duplication.
//
// This pins the contract from the tree's side: the entries it serves are the
// ones it was given, sanitised, and none of the work is done twice.
func TestListingIsAdoptedRatherThanCopied(t *testing.T) {
	given := []pan115.Entry{
		{ID: "f1", Name: "ok.mkv", Size: 1},
		{ID: "f2", Name: "with/slash.mkv", Size: 2},
		{ID: "f3", Name: "ok.mkv", Size: 3}, // a duplicate, to be disambiguated
	}
	handed := append([]pan115.Entry(nil), given...)

	dir := newDirectory(handed, time.Now().Add(time.Hour))

	// Same backing array: the listing was adopted, not duplicated.
	if len(dir.entries) != len(handed) || &dir.entries[0] != &handed[0] {
		t.Error("newDirectory copied the listing instead of taking it")
	}
	// And it is the sanitised, disambiguated view that is served.
	names := make([]string, len(dir.entries))
	for i, e := range dir.entries {
		names[i] = e.Name
	}
	want := []string{"ok.mkv", "with_slash.mkv", "ok (2).mkv"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, names[i], want[i])
		}
	}
	// Every name resolves to the entry it belongs to.
	for name, i := range dir.byName {
		if dir.entries[i].Name != name {
			t.Errorf("byName[%q] points at %q", name, dir.entries[i].Name)
		}
	}
}
