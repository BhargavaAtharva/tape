package api

import (
	"sync"
	"time"
)

// response is a finished HTTP answer that can be replayed to a retry.
type response struct {
	status int
	body   any
}

// inflight is one client order id's submission. done is closed once resp
// is set.
type inflight struct {
	done chan struct{}
	resp response
	at   time.Time
}

type clientKey struct {
	trader uint64
	id     string
}

// dedupe remembers client order ids for a window, so that a client whose
// request timed out can resend it and get the original outcome instead of
// placing a second order. It lives in the gateway's memory: ids are
// forgotten on restart, and after the window.
type dedupe struct {
	window time.Duration
	max    int

	mu    sync.Mutex
	m     map[clientKey]*inflight
	order []clientKey
}

func newDedupe(window time.Duration, max int) *dedupe {
	return &dedupe{window: window, max: max, m: make(map[clientKey]*inflight)}
}

// claim returns the entry for key and whether the caller created it and so
// must submit the order and call finish.
func (d *dedupe) claim(key clientKey, now time.Time) (*inflight, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	if e, ok := d.m[key]; ok {
		return e, false
	}
	e := &inflight{done: make(chan struct{}), at: now}
	d.m[key] = e
	d.order = append(d.order, key)
	return e, true
}

func (d *dedupe) finish(e *inflight, r response) {
	e.resp = r
	close(e.done)
}

// forget releases an id whose submission never reached the engine, so a
// retry is a fresh attempt.
func (d *dedupe) forget(key clientKey, e *inflight, r response) {
	d.mu.Lock()
	if d.m[key] == e {
		delete(d.m, key)
	}
	d.mu.Unlock()
	d.finish(e, r)
}

// pruneLocked drops ids older than the window, or the oldest ones beyond
// max, but never one whose submission is still in flight.
func (d *dedupe) pruneLocked(now time.Time) {
	for len(d.order) > 0 {
		k := d.order[0]
		if e := d.m[k]; e != nil {
			if now.Sub(e.at) < d.window && len(d.order) <= d.max {
				return
			}
			select {
			case <-e.done:
			default:
				return
			}
			delete(d.m, k)
		}
		d.order = d.order[1:]
	}
}
