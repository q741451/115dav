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
	// List returns the children of a directory. The slice becomes the
	// caller's: it is indexed and renamed in place rather than copied, which
	// on a large directory is the difference between one allocation and two.
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

const (
	// maxCachedEntries bounds the cache. It is entries rather than
	// directories because that is what occupies the space: a library scan
	// walks thousands of directories in minutes, and an empty one costs
	// nothing to hold. At roughly 200 bytes an entry this is some twelve
	// megabytes, affordable on the routers this is built for.
	//
	// A directory still counts as one, so that an account of nothing but
	// empty folders cannot fill the map without ever tripping the bound.
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

// A cached listing is read for two quite different reasons, and only one of
// them needs it to be current.
//
// Showing it to somebody does: a PROPFIND is how a player learns what is in a
// folder, and a file uploaded a minute ago should appear. Walking through it
// does not: there, the listing is only being asked which id the next path
// segment has, and that answer changes just when a directory is renamed or
// moved.
//
// Every path is walked from the root on every request, because 115 addresses
// by id and there is no usable stat-by-path. Making the walk insist on
// freshness therefore charged each seek up to one listing per directory in the
// path -- at two requests a second, seconds of silence before the first byte,
// every time the TTL lapsed. Letting it read what is already there costs
// nothing and is almost always right; forWalk marks those reads.
const (
	forDisplay = false
	forWalk    = true
)

// Origin is the listing a walk got its answer from, and whether that listing
// had expired.
//
// A walk that reads expired listings is nearly always right: an id and a pick
// code survive a rename and a move, and change only when the thing they name
// is deleted and something takes its place. When it is wrong, it is wrong
// because one particular listing is out of date -- so the cure is to refresh
// that one and try again, not to re-list the whole path.
type Origin struct {
	// Dir is the directory whose listing produced the entry, or, when a
	// segment was missing, the directory that should have contained it. When
	// listing a directory failed outright it is the directory that handed out
	// the id, since that is what turned out to be wrong.
	Dir   string
	Stale bool
}

// Lookup resolves a slash-separated path to an entry, walking from the root.
//
// stale is forWalk to prefer what is cached. It also reports where the answer
// came from, so that a caller who discovers the answer is wrong -- a pick code
// 115 will not serve, say -- can invalidate the one listing responsible.
func (t *Tree) Lookup(ctx context.Context, name string, stale bool) (pan115.Entry, Origin, error) {
	entry, from, err := t.walk(ctx, name, stale)
	if !from.Stale || !staleAnswer(err) {
		return entry, from, err
	}
	// The listing that gave this answer had expired, and the answer did not
	// work. Refresh that one and walk again; everything else still comes from
	// cache, so this costs exactly one listing. Once is enough -- the second
	// walk reads a listing it has just fetched, so it cannot ask again.
	t.Forget(from.Dir)
	return t.walk(ctx, name, stale)
}

// staleAnswer reports whether an error is the kind an out-of-date listing
// produces: a name that is no longer there, or an id 115 no longer knows.
func staleAnswer(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, pan115.ErrNotFound)
}

// unusableEntry widens staleAnswer to the way a file reports it: a pick code
// 115 acknowledges but will not serve. That is what a file replaced under the
// same name looks like from here.
func unusableEntry(err error) bool {
	var notDownloadable *pan115.ErrNotDownloadable
	return staleAnswer(err) || errors.As(err, &notDownloadable)
}

func (t *Tree) walk(ctx context.Context, name string, stale bool) (pan115.Entry, Origin, error) {
	current := t.Root()
	var from Origin
	for _, segment := range splitPath(name) {
		if !current.IsDir {
			return pan115.Entry{}, from, fs.ErrNotExist
		}
		dir, err := t.children(ctx, current.ID, stale)
		if err != nil {
			// This directory could not be listed at all -- most often because
			// 115 no longer has the id. The id came from the previous
			// listing, which from still names, and that is the one at fault.
			return pan115.Entry{}, from, err
		}
		from = Origin{Dir: current.ID, Stale: !time.Now().Before(dir.expires)}
		i, ok := dir.byName[segment]
		if !ok {
			return pan115.Entry{}, from, fs.ErrNotExist
		}
		current = dir.entries[i]
	}
	return current, from, nil
}

// Forget drops one directory's listing, so that the next read fetches it.
func (t *Tree) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir, ok := t.dirs[id]; ok {
		t.entries -= weigh(dir)
		delete(t.dirs, id)
	}
}

// Children lists a directory. stale is forWalk when the listing is only being
// used to translate a path, forDisplay when somebody is about to read it.
func (t *Tree) Children(ctx context.Context, id string, stale bool) ([]pan115.Entry, error) {
	dir, err := t.children(ctx, id, stale)
	if err != nil {
		return nil, err
	}
	return dir.entries, nil
}

func (t *Tree) children(ctx context.Context, id string, stale bool) (*directory, error) {
	if dir := t.cached(id, stale); dir != nil {
		return dir, nil
	}

	// Collapse concurrent misses so a burst of PROPFINDs costs one API call.
	loaded, err, _ := t.group.Do(id, func() (any, error) {
		// Deliberately not stale here, whoever asked. A caller willing to read
		// an expired listing has already been given it above, so reaching this
		// point means there is nothing cached at all -- and answering a caller
		// that does want a current one with somebody else's expired entry
		// would be exactly the sharing bug this collapse exists to avoid.
		if dir := t.cached(id, forDisplay); dir != nil {
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

func (t *Tree) cached(id string, stale bool) *directory {
	t.mu.Lock()
	defer t.mu.Unlock()
	dir, ok := t.dirs[id]
	if !ok {
		return nil
	}
	if stale || time.Now().Before(dir.expires) {
		return dir
	}
	return nil
}

// store adds a listing, emptying the cache first if it will not fit.
//
// Emptying it wholesale, rather than evicting a victim, is the deliberate
// part. A media library is walked in bursts, so a cold cache costs one rescan
// -- while any policy that chooses what to keep needs per-entry bookkeeping to
// avoid thrashing, and would be several times this much code to serve a case
// that arrives once in sixty directories.
//
// Clearing before the insert rather than after is what makes the listing just
// fetched survive: it is the one being read right now. A directory bigger than
// the whole budget is stored anyway and evicts itself on the next miss.
func (t *Tree) store(id string, dir *directory) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if previous, ok := t.dirs[id]; ok {
		t.entries -= weigh(previous)
		delete(t.dirs, id)
	}
	if t.entries+weigh(dir) > maxCachedEntries {
		clear(t.dirs)
		t.entries = 0
	}
	t.dirs[id] = dir
	t.entries += weigh(dir)
}

// weigh is what a cached listing costs against the budget. The extra one is
// the directory itself; see maxCachedEntries.
func weigh(dir *directory) int { return 1 + len(dir.entries) }

// newDirectory indexes a listing by name, making the names unique and safe to
// use as path segments so that the tree behaves like a real filesystem.
//
// It takes ownership of entries and renames them in place. Copying them into a
// second slice first doubled the cost of the one thing here that scales with
// the size of a directory, to no end: Backend.List returns a slice it does not
// keep, and this is its only caller.
func newDirectory(entries []pan115.Entry, expires time.Time) *directory {
	dir := &directory{
		entries: entries,
		byName:  make(map[string]int, len(entries)),
		expires: expires,
	}
	for i := range entries {
		entries[i].Name = uniqueName(dir.byName, sanitiseName(entries[i].Name))
		dir.byName[entries[i].Name] = i
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
