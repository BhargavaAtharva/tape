// Package engine runs one order book per symbol, each owned by a single
// goroutine. Commands reach a book only through its engine's inbound channel,
// so matching needs no locks and the order in which the engine dequeues
// commands is the one total order everything else is derived from.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/hist"
	"github.com/BhargavaAtharva/tape/wal"
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
	// Events, if set, receives every batch of events in sequence order,
	// starting with those regenerated while Open replays the log. It runs on
	// the engine goroutine and the slice must not be modified.
	Events func([]book.Event)
	// Clock stamps commands as they are dequeued. Defaults to wall time.
	Clock func() int64

	// Dir, if set, makes the engine durable: every command is appended to a
	// write-ahead log in Dir before it is applied, and Open rebuilds the book
	// from the newest snapshot there plus the log records after it.
	Dir string
	WAL wal.Options
	// SnapshotEvery is the number of logged commands between snapshots.
	SnapshotEvery uint64
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
	if c.SnapshotEvery == 0 {
		c.SnapshotEvery = 50_000
	}
}

// Result is what one command produced. Events is shared with other readers
// and must be treated as read-only.
type Result struct {
	Events []book.Event
}

type reply struct {
	res Result
	err error
}

type request struct {
	cmd   book.Command
	query func(*book.Book)
	reply chan reply
	enq   time.Time
}

// Recovery describes what Open found in the data directory.
type Recovery struct {
	SnapshotIndex uint64
	Replayed      uint64
	// LastIndex is the log index of the last command in the recovered state.
	LastIndex uint64
	Duration  time.Duration
}

type snapshotDone struct {
	index uint64
	err   error
}

type Engine struct {
	cfg  Config
	book *book.Book
	in   chan request

	quit      chan struct{}
	done      chan struct{}
	started   atomic.Bool
	closeOnce sync.Once

	log      *wal.Log
	record   []byte
	recovery Recovery
	// Snapshot bookkeeping, owned by the engine goroutine. Files are written
	// by a helper goroutine so a large book does not stall matching; at most
	// one write is in flight.
	snapIndex uint64
	prevSnap  uint64
	snapping  bool
	snapDone  chan snapshotDone
	failure   atomic.Pointer[error]
	closeErr  error

	latency  hist.Histogram
	commands atomic.Uint64
	fills    atomic.Uint64
	seq      atomic.Uint64
	lastSnap atomic.Uint64
}

func Open(cfg Config) (*Engine, error) {
	cfg.setDefaults()
	e := &Engine{
		cfg:      cfg,
		book:     book.New(cfg.IDBase),
		in:       make(chan request, cfg.QueueSize),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		snapDone: make(chan snapshotDone, 1),
	}
	if cfg.Dir != "" {
		if err := e.recover(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// recover loads the newest usable snapshot and replays the log after it.
// Replayed commands carry their original timestamps, so the regenerated
// events are byte-for-byte those of the original run.
func (e *Engine) recover() error {
	start := time.Now()
	index, state, err := wal.LoadSnapshot(e.cfg.Dir)
	if err != nil {
		return fmt.Errorf("engine %s: %w", e.cfg.Symbol, err)
	}
	if state != nil {
		b, err := book.Restore(state)
		if err != nil {
			return fmt.Errorf("engine %s: snapshot %d: %w", e.cfg.Symbol, index, err)
		}
		e.book = b
	}
	log, err := wal.Open(e.cfg.Dir, e.cfg.WAL)
	if err != nil {
		return fmt.Errorf("engine %s: %w", e.cfg.Symbol, err)
	}
	if index > log.LastIndex() {
		// The snapshot is newer than the surviving log, which can only happen
		// if unsynced log writes were lost. The snapshot is authoritative.
		slog.Warn("log ends before snapshot, restarting log", "symbol", e.cfg.Symbol, "log_last", log.LastIndex(), "snapshot", index)
		if err := log.SkipTo(index); err != nil {
			log.Close()
			return fmt.Errorf("engine %s: %w", e.cfg.Symbol, err)
		}
	}
	// Replayed events are handed out in batches of MaxBatch commands, the
	// same granularity as live traffic, rather than one call per command.
	var replayed uint64
	var pending []book.Event
	flush := func() {
		if len(pending) > 0 && e.cfg.Events != nil {
			e.cfg.Events(pending)
		}
		pending = nil
	}
	err = log.Replay(index, func(_ uint64, p []byte) error {
		cmd, err := book.DecodeCommand(p)
		if err != nil {
			return err
		}
		evs := e.book.Apply(cmd)
		e.countFills(evs)
		pending = append(pending, evs...)
		replayed++
		if replayed%uint64(e.cfg.MaxBatch) == 0 {
			flush()
		}
		return nil
	})
	flush()
	if err != nil {
		log.Close()
		return fmt.Errorf("engine %s: replay: %w", e.cfg.Symbol, err)
	}
	e.log = log
	e.snapIndex, e.prevSnap = index, index
	e.lastSnap.Store(index)
	e.seq.Store(e.book.Seq())
	e.recovery = Recovery{SnapshotIndex: index, Replayed: replayed, LastIndex: log.LastIndex(), Duration: time.Since(start)}
	return nil
}

// Recovery reports what Open restored. It is zero for a non-durable engine.
func (e *Engine) Recovery() Recovery { return e.recovery }

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

// Close stops the engine after it has processed everything already queued,
// then writes a final snapshot and closes the log.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		close(e.quit)
		if !e.started.Load() {
			e.shutdown()
		}
	})
	if e.started.Load() {
		<-e.done
	}
	return e.closeErr
}

// Submit queues a command and waits for its result.
func (e *Engine) Submit(ctx context.Context, cmd book.Command) (Result, error) {
	reply := make(chan reply, 1)
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
	reply := make(chan reply, 1)
	if err := e.send(ctx, request{query: fn, reply: reply}); err != nil {
		return err
	}
	_, err := e.await(ctx, reply)
	return err
}

func (e *Engine) send(ctx context.Context, r request) error {
	if p := e.failure.Load(); p != nil {
		return *p
	}
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

func (e *Engine) await(ctx context.Context, ch chan reply) (Result, error) {
	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-e.done:
		// The engine replies to everything it dequeued before closing done,
		// so an empty reply channel here means the request was never applied.
		select {
		case r := <-ch:
			return r.res, r.err
		default:
			return Result{}, ErrClosed
		}
	}
}

func (e *Engine) run() {
	defer close(e.done)
	defer e.shutdown()
	batch := make([]request, 0, e.cfg.MaxBatch)
	for {
		select {
		case r := <-e.in:
			batch = append(batch[:0], r)
		case s := <-e.snapDone:
			e.snapshotWritten(s)
			continue
		case <-e.quit:
			for {
				batch = e.fill(batch[:0])
				if len(batch) == 0 || !e.process(batch) {
					return
				}
			}
		}
		batch = e.fill(batch)
		if !e.process(batch) {
			return
		}
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

// process logs the batch's commands with one group commit, then applies
// them. Nothing is applied, and so nothing is visible to anyone, before it
// is in the log. It returns false if the log failed, after which the engine
// stops: continuing would let the book run ahead of what recovery can rebuild.
func (e *Engine) process(batch []request) bool {
	for i := range batch {
		if batch[i].query == nil {
			batch[i].cmd.Time = e.cfg.Clock()
		}
	}
	if e.log != nil {
		if err := e.logBatch(batch); err != nil {
			e.fail(err, batch)
			return false
		}
	}
	var out []book.Event
	for i := range batch {
		r := &batch[i]
		if r.query != nil {
			r.query(e.book)
			r.reply <- reply{}
			continue
		}
		evs := e.book.Apply(r.cmd)
		e.countFills(evs)
		out = append(out, evs...)
		if r.reply != nil {
			r.reply <- reply{res: Result{Events: evs}}
		}
		e.latency.Record(int64(time.Since(r.enq)))
		e.commands.Add(1)
	}
	if len(out) == 0 {
		return true
	}
	e.seq.Store(e.book.Seq())
	if e.cfg.Events != nil {
		e.cfg.Events(out)
	}
	if e.log != nil && !e.snapping && e.log.LastIndex()-e.snapIndex >= e.cfg.SnapshotEvery {
		if err := e.startSnapshot(); err != nil {
			e.fail(err, nil)
			return false
		}
	}
	return true
}

func (e *Engine) countFills(evs []book.Event) {
	var n uint64
	for i := range evs {
		if evs[i].Kind == book.EvFill {
			n++
		}
	}
	e.fills.Add(n)
}

func (e *Engine) logBatch(batch []request) error {
	for i := range batch {
		if batch[i].query != nil {
			continue
		}
		e.record = book.AppendCommand(e.record[:0], batch[i].cmd)
		if _, err := e.log.Append(e.record); err != nil {
			return err
		}
	}
	return e.log.Commit()
}

func (e *Engine) fail(err error, batch []request) {
	err = fmt.Errorf("engine %s: stopped: %w", e.cfg.Symbol, err)
	slog.Error("engine stopped", "symbol", e.cfg.Symbol, "err", err)
	e.failure.Store(&err)
	for i := range batch {
		if batch[i].reply != nil {
			batch[i].reply <- reply{err: err}
		}
	}
}

// startSnapshot encodes the book on the engine goroutine, where it is
// consistent, and rotates the log so that the records the snapshot covers
// end on a segment boundary and can later be deleted as whole files.
func (e *Engine) startSnapshot() error {
	index := e.log.LastIndex()
	state := e.book.AppendSnapshot(nil)
	if err := e.log.Rotate(); err != nil {
		return err
	}
	e.snapping = true
	e.snapIndex = index
	go func() {
		e.snapDone <- snapshotDone{index: index, err: wal.WriteSnapshot(e.cfg.Dir, index, state)}
	}()
	return nil
}

// snapshotWritten prunes old files once a snapshot is durable. It keeps the
// previous snapshot and the log since then, so a damaged newest snapshot can
// still be recovered from with a longer replay.
func (e *Engine) snapshotWritten(s snapshotDone) {
	e.snapping = false
	if s.err != nil {
		slog.Error("snapshot failed", "symbol", e.cfg.Symbol, "index", s.index, "err", s.err)
		return
	}
	e.lastSnap.Store(s.index)
	if err := e.log.RemoveThrough(e.prevSnap); err != nil {
		slog.Error("log cleanup failed", "symbol", e.cfg.Symbol, "err", err)
	}
	if indexes, err := wal.ListSnapshots(e.cfg.Dir); err == nil {
		for _, i := range indexes {
			if i < e.prevSnap {
				if err := wal.RemoveSnapshot(e.cfg.Dir, i); err != nil {
					slog.Error("snapshot cleanup failed", "symbol", e.cfg.Symbol, "err", err)
				}
			}
		}
	}
	e.prevSnap = s.index
}

// shutdown runs once, on the engine goroutine (or in Close if the engine
// never started). After a clean stop it leaves a snapshot of the final state
// so the next start has nothing to replay.
func (e *Engine) shutdown() {
	if e.log == nil {
		return
	}
	if e.snapping {
		e.snapshotWritten(<-e.snapDone)
	}
	if e.failure.Load() != nil {
		e.log.Abandon()
		return
	}
	if e.log.LastIndex() > e.snapIndex {
		index := e.log.LastIndex()
		if err := e.log.Sync(); err != nil {
			e.closeErr = err
		} else if err := wal.WriteSnapshot(e.cfg.Dir, index, e.book.AppendSnapshot(nil)); err != nil {
			e.closeErr = err
		} else {
			e.snapIndex = index
			e.snapshotWritten(snapshotDone{index: index})
		}
	}
	if err := e.log.Close(); err != nil && e.closeErr == nil {
		e.closeErr = err
	}
}

// Stats is a snapshot of the engine's counters, safe to take at any time.
// WALWritten and WALSynced are log indexes; their difference is the number
// of commands applied but not yet on stable storage.
type Stats struct {
	Commands   uint64
	Fills      uint64
	Seq        uint64
	QueueDepth int
	Latency    *hist.Counts

	Durable       bool
	WALWritten    uint64
	WALSynced     uint64
	SnapshotIndex uint64
}

func (e *Engine) Stats() Stats {
	st := Stats{
		Commands:   e.commands.Load(),
		Fills:      e.fills.Load(),
		Seq:        e.seq.Load(),
		QueueDepth: len(e.in),
		Latency:    e.latency.Snapshot(),
	}
	if e.log != nil {
		st.Durable = true
		st.WALWritten = e.log.Written()
		st.WALSynced = e.log.Synced()
		st.SnapshotIndex = e.lastSnap.Load()
	}
	return st
}
