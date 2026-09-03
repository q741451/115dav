package dav

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/q741451/115dav/internal/pan115"
)

// Backend is the slice of 115 this package needs. The concrete implementation
// is *pan115.Client; the interface exists so the WebDAV layer can be tested
// without talking to 115.
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
	backend Backend
	rootID  string
	ttl     time.Duration

	group singleflight.Group
	mu    sync.Mutex
	dirs  map[string]*directory
}

// maxCachedDirs bounds memory on large accounts. Reaching it triggers a sweep
// of expired entries before anything live is dropped.
const maxCachedDirs = 4096

type directory struct {
	entries []pan115.Entry
	byName  map[string]pan115.Entry
	expires time.Time
}

// NewTree returns a Tree rooted at the given 115 category id. An empty rootID
// means the account root.
func NewTree(backend Backend, rootID string, ttl time.Duration) *Tree {
	if rootID == "" {
		rootID = pan115.RootID
	}
	return &Tree{
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
		child, ok := dir.byName[segment]
		if !ok {
			return pan115.Entry{}, fs.ErrNotExist
		}
		current = child
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
		entries, err := t.backend.List(ctx, id)
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
	if len(t.dirs) >= maxCachedDirs {
		t.sweepLocked()
	}
	t.dirs[id] = dir
}

// sweepLocked drops expired listings, and failing that the whole cache. A
// media library is walked in bursts, so a cold cache costs one rescan rather
// than the steady thrash a strict LRU would need bookkeeping to avoid.
func (t *Tree) sweepLocked() {
	now := time.Now()
	for id, dir := range t.dirs {
		if now.After(dir.expires) {
			delete(t.dirs, id)
		}
	}
	if len(t.dirs) >= maxCachedDirs {
		clear(t.dirs)
	}
}

// Forget drops any cached listing for a directory.
func (t *Tree) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.dirs, id)
}

// newDirectory indexes a listing by name, making the names unique and safe to
// use as path segments so that the tree behaves like a real filesystem.
func newDirectory(entries []pan115.Entry, expires time.Time) *directory {
	dir := &directory{
		entries: make([]pan115.Entry, 0, len(entries)),
		byName:  make(map[string]pan115.Entry, len(entries)),
		expires: expires,
	}
	for _, entry := range entries {
		entry.Name = uniqueName(dir.byName, sanitiseName(entry.Name))
		dir.entries = append(dir.entries, entry)
		dir.byName[entry.Name] = entry
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
func uniqueName(taken map[string]pan115.Entry, name string) string {
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
