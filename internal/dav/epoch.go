package dav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/q741451/115dav/internal/pan115"
)

// An epoch is one set of 115 credentials and everything derived from them.
//
// Nothing in it is mutated after construction. Replacing the credentials means
// building a new epoch and swapping the pointer, never editing this one -- and
// that is what makes the swap safe. A listing that was already in flight when
// the swap happened stores its result in the epoch it was started under, which
// by then nobody can reach, so it is collected rather than served.
//
// The alternative, which this replaces, was to keep one long-lived cache and
// empty it when the credentials changed. That needs the emptying to be ordered
// against every fetch that might still land, and it was not: a listing made
// with the old cookies could be stored after the flush and served for a full
// dir-ttl afterwards -- from an account the channel may no longer even point
// at.
type epoch struct {
	backend Backend
	tree    *Tree
	links   *linkCache

	// ctx bounds the shared work this epoch owns -- listings and resolves,
	// which belong to the process rather than to whichever request happened to
	// trigger them. Retiring the epoch cancels it. Streaming a file does not
	// use it: those bytes belong to one client, and run on its request context.
	ctx    context.Context
	cancel context.CancelFunc
}

func newEpoch(owner context.Context, backend Backend, opts Options) *epoch {
	ctx, cancel := context.WithCancel(owner)
	return &epoch{
		backend: backend,
		tree:    NewTree(ctx, backend, opts.RootID, opts.DirTTL),
		links:   newLinkCache(ctx, backend, opts.LinkTTL),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Credentials supplies the 115 client requests work through, and is told when
// 115 stops accepting one.
//
// It is an interface here and implemented elsewhere so that this package knows
// nothing about cookies, cookie servers or how a login is replaced -- and so
// that the package which does knows nothing about HTTP.
type Credentials interface {
	// Backend returns the client to use now, fetching credentials if it has
	// none. An error means nothing usable is available; the request is refused
	// with 503 and the error is logged, so it should say why.
	Backend(ctx context.Context) (Backend, error)

	// Reject reports that 115 refused this backend's credentials. The next
	// call to Backend is expected to produce different ones, or an error.
	Reject(backend Backend)
}

// Static is the Credentials of a mount whose cookie was given on the command
// line: there is one client, it never changes, and when 115 stops accepting it
// there is nothing to be done but say so.
type Static struct{ B Backend }

func (s Static) Backend(context.Context) (Backend, error) { return s.B, nil }
func (s Static) Reject(Backend)                           {}

// errNoCredentials is what a caller sees when the source cannot supply any.
var errNoCredentials = errors.New("no usable 115 credentials")

// epochs holds the current epoch, and is the only mutable state the server has.
type epochs struct {
	owner context.Context
	creds Credentials
	opts  Options
	log   *slog.Logger

	// mu serialises building, so that a burst of requests arriving with no
	// epoch fetches credentials once rather than once each.
	mu      sync.Mutex
	current *epoch
}

// get returns the current epoch, building one if there is none.
func (e *epochs) get(ctx context.Context) (*epoch, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil {
		return e.current, nil
	}
	if err := e.owner.Err(); err != nil {
		return nil, err
	}

	backend, err := e.creds.Backend(ctx)
	if err != nil {
		// Whatever the source could not do, the answer here is the same: there
		// is nothing to serve with, and it may work later. Wrapping rather
		// than requiring a shared sentinel is what lets the source know
		// nothing about this package -- its own reason survives, for the log.
		return nil, fmt.Errorf("%w: %w", errNoCredentials, err)
	}
	e.current = newEpoch(e.owner, backend, e.opts)
	return e.current, nil
}

// retire drops the epoch if it is still the current one, and cancels the work
// it owns.
//
// The comparison is what makes concurrent reports of the same failure safe:
// several requests will notice one expired login at once, and only the first
// of them replaces anything. Comparing pointers rather than cookies also means
// no part of this has to know what a credential looks like.
func (e *epochs) retire(stale *epoch) {
	e.mu.Lock()
	replaced := e.current == stale
	if replaced {
		e.current = nil
	}
	e.mu.Unlock()

	if replaced {
		stale.cancel()
	}
}

// close retires whatever is current, for shutdown.
func (e *epochs) close() {
	e.mu.Lock()
	stale := e.current
	e.current = nil
	e.mu.Unlock()
	if stale != nil {
		stale.cancel()
	}
}

// withEpoch runs op against the current epoch, and once more against a fresh
// one if 115 rejects the credentials this one was built from.
//
// A request therefore never straddles two sets of credentials: whatever it
// looked up and whatever it then resolves come from the same account. A method
// cannot take a type parameter, hence the free function.
func withEpoch[T any](e *epochs, ctx context.Context, op func(*epoch) (T, error)) (T, error) {
	var zero T

	current, err := e.get(ctx)
	if err != nil {
		return zero, err
	}

	result, err := op(current)
	if !errors.Is(err, pan115.ErrNotAuthorized) {
		return result, err
	}

	// 115 no longer accepts what this epoch was built from. Everything derived
	// from it goes with it; whatever the source produces next is a clean start,
	// possibly on a different account entirely.
	e.log.Info("115 rejected the current credentials, asking for new ones")
	e.creds.Reject(current.backend)
	e.retire(current)

	fresh, err := e.get(ctx)
	if err != nil {
		return zero, err
	}
	if fresh == current {
		// The source handed back the credentials 115 just refused. Trying them
		// again would only earn the same answer.
		return zero, errNoCredentials
	}
	return op(fresh)
}
