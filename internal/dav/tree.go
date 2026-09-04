package dav

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/q741451/115dav/internal/pan115"
)

// ErrUnavailable means the backend cannot serve right now but may later --
// the credentials it uses are being replaced, say.
//
// It is answered with 503 rather than an empty listing on purpose. To a media
// client an empty directory is a fact: it will conclude the library is gone
// and discard what it knows about it. A 503 says "ask again", which is what
// is actually true.
var ErrUnavailable = errors.New("115 is temporarily unavailable")

// Backend is the slice of 115 this package needs. The concrete implementation
// is *pan115.Client, or a wrapper that keeps its credentials current; the
// interface exists so the WebDAV layer can be tested without talking to 115.
type Backend interface {
	List(ctx context.Context, id string) ([]pan115.Entry, error)
	Resolve(ctx context.Context, pickCode string) (*pan115.Target, error)
}

// Tree presents the account as a path-addressed, read-only filesystem.
//
// 115 addresses directories by id, so every lookup walks from the root one
// segment at a time. Listings are cached, which is what keeps that walk cheap
// and keeps a media scanner from melting the API rate limit.
type Tree struct {
	// owner bounds the listings this tree fetches. It belongs to the epoch
	// that built the tree, so retiring those credentials cancels the work
	// begun under them rather than leaving it to land in a cache nobody reads.
	owner   context.Context
	backend Backend
	rootID  string
	ttl     time.Duration

	group singleflight.Group

	mu      sync.Mutex
	dirs    map[string]*directory
	entries int // total across dirs, kept in step with the map below
}

// Bounds on the cache. Directory count alone says nothing about memory: a
// library scan walks thousands of directories in minutes, and it is the
// entries inside them that occupy the space. At roughly 200 bytes an entry
// the ceiling below is on the order of ten megabytes, which is affordable on
// the routers this is built for.
const (
	maxCachedDirs    = 4096
	maxCachedEntries = 60000

	// listTimeout bounds a listing that no longer has a caller waiting for
	// it. Paginating a very large directory against the rate limit is slow,
	// so it is generous.
	listTimeout = 2 * time.Minute
)

type directory struct {
	entries []pan115.Entry
	// byName indexes into entries rather than copying them: a second copy of
	// every entry would double what a cached listing costs.
	byName  map[string]int
	expires time.Time
}

// NewTree returns a Tree rooted at the given 115 category id. An empty rootID
// means the account root.
func NewTree(owner context.Context, backend Backend, rootID string, ttl time.Duration) *Tree {
	if rootID == "" {
		rootID = pan115.RootID
	}
	return &Tree{
		owner:   owner,
		backend: backend,
		rootID:  rootID,
		ttl:     ttl,
		dirs:    map[string]*directory{},
	}
}

// Root is the synthetic entry standing in for the mount point itself.
func (t *Tree) Root() pan115.Entry {
	return pan115.Entry{ID: t.rootID, Name: "/", IsDir: true}
}

// Lookup resolves a slash-separated path to an entry, walking from the root.
func (t *Tree) Lookup(ctx context.Context, name string) (pan115.Entry, error) {
	current := t.Root()
	for _, segment := range splitPath(name) {
		if !current.IsDir {
			return pan115.Entry{}, fs.ErrNotExist
		}
		dir, err := t.children(ctx, current.ID)
		if err != nil {
			return pan115.Entry{}, err
		}
		i, ok := dir.byName[segment]
		if !ok {
			return pan115.Entry{}, fs.ErrNotExist
		}
		current = dir.entries[i]
	}
	return current, nil
}

// Children lists a directory, serving from cache when it is still fresh.
func (t *Tree) Children(ctx context.Context, id string) ([]pan115.Entry, error) {
	dir, err := t.children(ctx, id)
	if err != nil {
		return nil, err
	}
	return dir.entries, nil
}

func (t *Tree) children(ctx context.Context, id string) (*directory, error) {
	if dir := t.cached(id); dir != nil {
		return dir, nil
	}

	// Collapse concurrent misses so a burst of PROPFINDs costs one API call.
	loaded, err, _ := t.group.Do(id, func() (any, error) {
		if dir := t.cached(id); dir != nil {
			return dir, nil
		}
		// The listing is shared with everyone else waiting on this id, so it
		// is not tied to whichever request happened to start it: that caller
		// may hang up -- players do, constantly -- and the rest would inherit
		// its cancellation. It hangs off the epoch instead, which is what the
		// listing actually belongs to.
		fetch, cancel := context.WithTimeout(t.owner, listTimeout)
		defer cancel()

		entries, err := t.backend.List(fetch, id)
		if err != nil {
			return nil, err
		}
		dir := newDirectory(entries, time.Now().Add(t.ttl))
		t.store(id, dir)
		return dir, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.(*directory), nil
}

func (t *Tree) cached(id string) *directory {
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir, ok := t.dirs[id]; ok && time.Now().Before(dir.expires) {
		return dir
	}
	return nil
}

func (t *Tree) store(id string, dir *directory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if previous, ok := t.dirs[id]; ok {
		t.entries -= len(previous.entries)
	}
	t.dirs[id] = dir
	t.entries += len(dir.entries)
	if t.overLimitLocked() {
		t.sweepLocked(id)
	}
}

func (t *Tree) overLimitLocked() bool {
	return len(t.dirs) > maxCachedDirs || t.entries > maxCachedEntries
}

// sweepLocked drops expired listings, and failing that everything except the
// directory just stored -- which is the one being read right now. A media
// library is walked in bursts, so a cold cache costs one rescan rather than
// the steady thrash a strict LRU would need bookkeeping to avoid.
func (t *Tree) sweepLocked(keep string) {
	now := time.Now()
	for id, dir := range t.dirs {
		if now.After(dir.expires) {
			delete(t.dirs, id)
			t.entries -= len(dir.entries)
		}
	}
	if !t.overLimitLocked() {
		return
	}

	kept := t.dirs[keep]
	clear(t.dirs)
	t.entries = 0
	if kept != nil {
		// A single directory larger than the whole budget is still worth
		// keeping: it is already in memory, and dropping it would mean
		// listing it again on the very next request.
		t.dirs[keep] = kept
		t.entries = len(kept.entries)
	}
}

// Forget drops any cached listing for a directory.
func (t *Tree) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir, ok := t.dirs[id]; ok {
		t.entries -= len(dir.entries)
		delete(t.dirs, id)
	}
}

// newDirectory indexes a listing by name, making the names unique and safe to
// use as path segments so that the tree behaves like a real filesystem.
func newDirectory(entries []pan115.Entry, expires time.Time) *directory {
	dir := &directory{
		entries: make([]pan115.Entry, 0, len(entries)),
		byName:  make(map[string]int, len(entries)),
		expires: expires,
	}
	for _, entry := range entries {
		entry.Name = uniqueName(dir.byName, sanitiseName(entry.Name))
		dir.byName[entry.Name] = len(dir.entries)
		dir.entries = append(dir.entries, entry)
	}
	return dir
}

// sanitiseName removes characters that cannot survive a path segment.
func sanitiseName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	// "." and ".." would resolve against the tree rather than naming a child.
	switch strings.TrimSpace(name) {
	case "", ".", "..":
		return "_" + name
	}
	return name
}

// uniqueName suffixes duplicates, keeping the extension last so media scanners
// still recognise the format.
func uniqueName(taken map[string]int, name string) string {
	if _, clash := taken[name]; !clash {
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
}

// splitPath breaks a WebDAV path into non-empty segments.
func splitPath(name string) []string {
	name = path.Clean("/" + strings.TrimPrefix(name, "/"))
	if name == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(name, "/"), "/")
}
