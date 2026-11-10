package service

import (
	"context"
	"sync"
)

// keyedMutex hands out one lock per resource identifier. It replaces the old
// service-wide mutex so a long render cannot block unrelated source or
// timeline operations.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyEntry
}

type keyEntry struct {
	lock sync.Mutex
	refs int
}

func (km *keyedMutex) Lock(key string) func() {
	km.mu.Lock()
	if km.entries == nil {
		km.entries = make(map[string]*keyEntry)
	}
	entry, ok := km.entries[key]
	if !ok {
		entry = &keyEntry{}
		km.entries[key] = entry
	}
	entry.refs++
	km.mu.Unlock()

	entry.lock.Lock()
	return func() {
		entry.lock.Unlock()
		km.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(km.entries, key)
		}
		km.mu.Unlock()
	}
}

// flight represents one in-flight operation for a resource id.
type flight struct {
	done chan struct{}
}

// flightMap deduplicates concurrent operations on the same id. The first
// caller runs; concurrent callers wait and observe the same outcome, which
// prevents duplicate render pipelines and contradictory receipts.
type flightMap struct {
	mu      sync.Mutex
	pending map[string]*flight
}

func (fm *flightMap) begin(key string) (*flight, bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if fm.pending == nil {
		fm.pending = make(map[string]*flight)
	}
	if active, ok := fm.pending[key]; ok {
		return active, false
	}
	f := &flight{done: make(chan struct{})}
	fm.pending[key] = f
	return f, true
}

func (fm *flightMap) finish(key string, f *flight) {
	close(f.done)
	fm.mu.Lock()
	delete(fm.pending, key)
	fm.mu.Unlock()
}

func (fm *flightMap) wait(ctx context.Context, f *flight) error {
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HoldRenderSlotForTest occupies one render worker slot and returns a release
// function. It lets transport-layer tests force the capacity-saturation path
// deterministically; production code must not call it.
func (s *Service) HoldRenderSlotForTest() func() {
	s.renderSlots <- struct{}{}
	return func() { <-s.renderSlots }
}
