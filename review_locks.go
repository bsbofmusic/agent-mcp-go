package main

import (
	"context"
	"sort"
	"sync"
)

type projectLock struct {
	gate chan struct{}
	refs int
}
type projectLockPool struct {
	mu    sync.Mutex
	locks map[string]*projectLock
}

// Locks exist only while held or awaited. Repository keys serialize cooperating
// tool calls without blocking unrelated projects or retaining an idle registry.
func (p *projectLockPool) acquire(ctx context.Context, keys []string) (func(), error) {
	ordered := append([]string(nil), keys...)
	sort.Strings(ordered)
	var releases []func()
	unwind := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	previous := ""
	for i, key := range ordered {
		if i > 0 && key == previous {
			continue
		}
		previous = key
		p.mu.Lock()
		if p.locks == nil {
			p.locks = map[string]*projectLock{}
		}
		lock := p.locks[key]
		if lock == nil {
			lock = &projectLock{gate: make(chan struct{}, 1)}
			p.locks[key] = lock
		}
		lock.refs++
		p.mu.Unlock()
		dropRef := func() {
			p.mu.Lock()
			lock.refs--
			if lock.refs == 0 {
				delete(p.locks, key)
			}
			p.mu.Unlock()
		}
		select {
		case lock.gate <- struct{}{}:
			releases = append(releases, func() { <-lock.gate; dropRef() })
		case <-ctx.Done():
			dropRef()
			unwind()
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		unwind()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(unwind) }, nil
}
