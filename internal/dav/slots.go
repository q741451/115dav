package dav

import (
	"context"
	"log/slog"
	"sync"
)

// 115 refuses a file it is already serving too many times at once.
//
// Measured against a real account: two simultaneous range requests on one file
// are served, a third is refused with "115 pmt 3-2", and the refusals compound
// -- six at once got fewer through than three did. Sequential requests are
// never refused, however fast and whether or not the connection is reused, so
// what is limited is what is open at the same time rather than how often we
// ask.
//
// A limit on simultaneous work is a semaphore, and this is one, per file.
const maxStreamsPerFile = 2

// slots admits streams to a file, evicting the oldest to make room.
//
// Newest wins, always. The alternative is to queue, and queueing needs to know
// whether the stream ahead is alive -- which cannot be told from a stream
// whose player has buffered ahead and stopped reading, and gets it wrong in
// the direction of stalling a player that was fine. Evicting instead needs to
// know nothing: the newest request is the one someone is waiting on now.
//
// It costs the case where two players seek around the same file at the same
// moment, which then take turns interrupting each other. That is accepted:
// one file being played twice over at once is rare, and every alternative
// pays for it with a timeout to tune and a way to guess wrong.
//
// The common case is not that at all. It is one player seeking, where the
// stream being evicted is that same player's abandoned one -- already dead,
// still holding 115's slot until the transport notices.
type slots struct {
	log *slog.Logger
	// limit is maxStreamsPerFile except in tests, which need to exercise the
	// paths either side of it without being at 115's number.
	limit int

	mu   sync.Mutex
	held map[string][]*slot // by pick code, oldest first
}

// slot is one admitted stream. Cancelling it aborts the upstream request,
// which is what frees the file at 115.
type slot struct {
	pickCode string
	cancel   context.CancelFunc
}

func newSlots(log *slog.Logger) *slots {
	return &slots{log: log, limit: maxStreamsPerFile, held: map[string][]*slot{}}
}

// acquire admits a stream for pickCode and returns a context to make the
// upstream request with. Cancelling that context is how the stream is evicted,
// so it must be the one the request uses -- not the caller's own.
//
// The returned release must be called when the stream ends.
func (s *slots) acquire(ctx context.Context, pickCode, name string) (context.Context, *slot, func()) {
	streamCtx, cancel := context.WithCancel(ctx)
	mine := &slot{pickCode: pickCode, cancel: cancel}

	s.mu.Lock()
	holders := s.held[pickCode]
	var evicted []*slot
	for len(holders) >= s.limit {
		evicted = append(evicted, holders[0])
		holders = holders[1:]
	}
	s.held[pickCode] = append(holders, mine)
	s.mu.Unlock()

	// Outside the lock: a cancelled stream wakes its own goroutine, which will
	// want this lock to release itself.
	for _, old := range evicted {
		s.log.Debug("evicting an older stream of the same file",
			"file", name, "limit", s.limit)
		old.cancel()
	}

	return streamCtx, mine, func() { s.release(mine) }
}

// release gives up a slot. It is safe to call after the slot has been evicted,
// and safe to call twice.
func (s *slots) release(mine *slot) {
	s.mu.Lock()
	holders := s.held[mine.pickCode]
	for i, held := range holders {
		if held == mine {
			holders = append(holders[:i], holders[i+1:]...)
			break
		}
	}
	if len(holders) == 0 {
		// Otherwise the map grows by one entry per file ever streamed.
		delete(s.held, mine.pickCode)
	} else {
		s.held[mine.pickCode] = holders
	}
	s.mu.Unlock()

	// Idempotent, and required: releasing without cancelling would leave the
	// upstream request running with nobody reading it.
	mine.cancel()
}

// evicted reports whether this slot was taken away, as opposed to the client
// going home. The two are indistinguishable from the copy loop -- both arrive
// as a cancelled context -- but only one of them is worth logging as ours.
func (s *slots) evicted(mine *slot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, held := range s.held[mine.pickCode] {
		if held == mine {
			return false
		}
	}
	return true
}
