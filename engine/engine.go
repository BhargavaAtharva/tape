// Package engine runs one order book per symbol, each owned by a single
// goroutine. Commands reach a book only through its engine's inbound channel,
// so matching needs no locks and the order in which the engine dequeues
// commands is the one total order everything else is derived from.
package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/hist"
)

var ErrClosed = errors.New("engine: closed")

type Config struct {
	Symbol string
	// IDBase is added to every order id this engine assigns.
	IDBase uint64
	// QueueSize bounds the inbound channel. Submitters block when it is full.
	QueueSize int
	// MaxBatch caps how many queued commands are processed as one batch.
	MaxBatch int
	// Events, if set, receives every batch of events in sequence order. It
	// runs on the engine goroutine and the slice must not be modified.
	Events func([]book.Event)
	// Clock stamps commands as they are dequeued. Defaults to wall time.
	Clock func() int64
}

func (c *Config) setDefaults() {
	if c.QueueSize <= 0 {
		c.QueueSize = 4096
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 256
	}
	if c.Clock == nil {
		c.Clock = func() int64 { return time.Now().UnixNano() }
	}
}

// Result is what one command produced. Events is shared with other readers
// and must be treated as read-only.
type Result struct {
	Events []book.Event
}

type request struct {
	cmd   book.Command
	query func(*book.Book)
	reply chan Result
	enq   time.Time
}

type Engine struct {
	cfg  Config
	book *book.Book
	in   chan request

	quit      chan struct{}
	done      chan struct{}
	started   atomic.Bool
	closeOnce sync.Once

	latency  hist.Histogram
	commands atomic.Uint64
	fills    atomic.Uint64
	seq      atomic.Uint64
}

func Open(cfg Config) (*Engine, error) {
	cfg.setDefaults()
	e := &Engine{
		cfg:  cfg,
		book: book.New(cfg.IDBase),
		in:   make(chan request, cfg.QueueSize),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	return e, nil
}

func (e *Engine) Symbol() string { return e.cfg.Symbol }

// Start launches the engine goroutine. Until then the book can be inspected
// directly through Book.
func (e *Engine) Start() {
	if e.started.CompareAndSwap(false, true) {
		go e.run()
	}
}

// Book exposes the book for inspection before Start and after Close. While
// the engine is running, use Query instead.
func (e *Engine) Book() *book.Book { return e.book }

// Close stops the engine after it has processed everything already queued.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() { close(e.quit) })
	if e.started.Load() {
		<-e.done
	}
	return nil
}

// Submit queues a command and waits for its result.
func (e *Engine) Submit(ctx context.Context, cmd book.Command) (Result, error) {
	reply := make(chan Result, 1)
	if err := e.send(ctx, request{cmd: cmd, reply: reply, enq: time.Now()}); err != nil {
		return Result{}, err
	}
	return e.await(ctx, reply)
}

// Enqueue queues a command without waiting for the result.
func (e *Engine) Enqueue(ctx context.Context, cmd book.Command) error {
	return e.send(ctx, request{cmd: cmd, enq: time.Now()})
}

// Query runs fn on the engine goroutine, between commands, with exclusive
// access to the book. fn must not retain the book or modify it.
func (e *Engine) Query(ctx context.Context, fn func(*book.Book)) error {
	reply := make(chan Result, 1)
	if err := e.send(ctx, request{query: fn, reply: reply}); err != nil {
		return err
	}
	_, err := e.await(ctx, reply)
	return err
}

func (e *Engine) send(ctx context.Context, r request) error {
	select {
	case <-e.quit:
		return ErrClosed
	default:
	}
	select {
	case e.in <- r:
		return nil
	case <-e.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) await(ctx context.Context, reply chan Result) (Result, error) {
	select {
	case res := <-reply:
		return res, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-e.done:
		// The engine replies to everything it dequeued before closing done,
		// so an empty reply channel here means the request was never applied.
		select {
		case res := <-reply:
			return res, nil
		default:
			return Result{}, ErrClosed
		}
	}
}

func (e *Engine) run() {
	defer close(e.done)
	batch := make([]request, 0, e.cfg.MaxBatch)
	for {
		select {
		case r := <-e.in:
			batch = append(batch[:0], r)
		case <-e.quit:
			for {
				batch = e.fill(batch[:0])
				if len(batch) == 0 {
					return
				}
				e.process(batch)
			}
		}
		batch = e.fill(batch)
		e.process(batch)
	}
}

// fill tops the batch up with whatever is already queued, without waiting.
func (e *Engine) fill(batch []request) []request {
	for len(batch) < e.cfg.MaxBatch {
		select {
		case r := <-e.in:
			batch = append(batch, r)
		default:
			return batch
		}
	}
	return batch
}

func (e *Engine) process(batch []request) {
	var out []book.Event
	var fills uint64
	for i := range batch {
		r := &batch[i]
		if r.query != nil {
			r.query(e.book)
			r.reply <- Result{}
			continue
		}
		r.cmd.Time = e.cfg.Clock()
		evs := e.book.Apply(r.cmd)
		for j := range evs {
			if evs[j].Kind == book.EvFill {
				fills++
			}
		}
		out = append(out, evs...)
		if r.reply != nil {
			r.reply <- Result{Events: evs}
		}
		e.latency.Record(int64(time.Since(r.enq)))
		e.commands.Add(1)
	}
	if len(out) == 0 {
		return
	}
	e.fills.Add(fills)
	e.seq.Store(e.book.Seq())
	if e.cfg.Events != nil {
		e.cfg.Events(out)
	}
}

// Stats is a snapshot of the engine's counters, safe to take at any time.
type Stats struct {
	Commands   uint64
	Fills      uint64
	Seq        uint64
	QueueDepth int
	Latency    *hist.Counts
}

func (e *Engine) Stats() Stats {
	return Stats{
		Commands:   e.commands.Load(),
		Fills:      e.fills.Load(),
		Seq:        e.seq.Load(),
		QueueDepth: len(e.in),
		Latency:    e.latency.Snapshot(),
	}
}
