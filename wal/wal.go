// Package wal is an append-only, segmented write-ahead log.
//
// A segment file is a 16-byte header followed by records:
//
//	header:  "TAPEWAL1" | first index (u64)
//	record:  payload length (u32) | CRC-32C (u32) | index (u64) | payload
//
// All integers are little-endian. The checksum covers the index and the
// payload. Indexes are assigned by the log, start at 1 and are contiguous
// across segments.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	segmentMagic  = "TAPEWAL1"
	headerSize    = 16
	recordHeader  = 16
	maxPayload    = 1 << 16
	segmentPrefix = "wal-"
	segmentSuffix = ".log"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt reports damage that cannot be explained by a crash during the
// last write: a bad record or a missing range before the tail of the log.
var ErrCorrupt = errors.New("wal: corrupt log")

type SyncPolicy int

const (
	// SyncBatch fsyncs on every Commit. Nothing acknowledged is ever lost.
	SyncBatch SyncPolicy = iota
	// SyncInterval fsyncs from a background goroutine. A process crash loses
	// nothing; a power failure can lose up to one interval of commits.
	SyncInterval
	// SyncNone leaves flushing to the operating system.
	SyncNone
)

func ParseSyncPolicy(s string) (SyncPolicy, error) {
	switch strings.ToLower(s) {
	case "batch":
		return SyncBatch, nil
	case "interval":
		return SyncInterval, nil
	case "none":
		return SyncNone, nil
	}
	return 0, fmt.Errorf("wal: unknown sync policy %q (want batch, interval or none)", s)
}

func (p SyncPolicy) String() string {
	switch p {
	case SyncBatch:
		return "batch"
	case SyncInterval:
		return "interval"
	}
	return "none"
}

type Options struct {
	Sync SyncPolicy
	// Interval is the fsync period for SyncInterval. Defaults to 10ms.
	Interval time.Duration
}

type segment struct {
	first uint64
	path  string
}

// Log is written by one goroutine. Only the background syncer of
// SyncInterval runs concurrently with it, and it touches nothing but the
// current file handle (under mu) and the atomic counters.
type Log struct {
	dir  string
	opts Options

	segments []segment
	buf      []byte
	last     uint64

	mu sync.Mutex // guards f against the syncer during rotation
	f  *os.File

	written  atomic.Uint64
	synced   atomic.Uint64
	syncErr  atomic.Pointer[error]
	stop     chan struct{}
	syncDone chan struct{}
}

func segmentName(first uint64) string {
	return fmt.Sprintf("%s%020d%s", segmentPrefix, first, segmentSuffix)
}

// Open opens the log in dir, creating it if needed. A torn or corrupt tail
// in the newest segment is cut off; everything before it is kept.
func Open(dir string, opts Options) (*Log, error) {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Millisecond
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}
	segs, err := listSegments(dir)
	if err != nil {
		return nil, err
	}
	l := &Log{dir: dir, opts: opts, segments: segs}
	if len(segs) == 0 {
		if err := l.newSegment(1); err != nil {
			return nil, err
		}
	} else if err := l.openTail(); err != nil {
		return nil, err
	}
	l.written.Store(l.last)
	l.synced.Store(l.last)
	if opts.Sync == SyncInterval {
		l.stop = make(chan struct{})
		l.syncDone = make(chan struct{})
		go l.syncLoop()
	}
	return l, nil
}

func listSegments(dir string) ([]segment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: list dir: %w", err)
	}
	var segs []segment
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		var first uint64
		if _, err := fmt.Sscanf(name, segmentPrefix+"%020d"+segmentSuffix, &first); err != nil || first == 0 {
			return nil, fmt.Errorf("%w: unexpected file %s", ErrCorrupt, name)
		}
		segs = append(segs, segment{first: first, path: filepath.Join(dir, name)})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	return segs, nil
}

// openTail validates the newest segment, truncates it after the last good
// record and leaves the file positioned for appending.
func (l *Log) openTail() error {
	tail := l.segments[len(l.segments)-1]
	data, err := os.ReadFile(tail.path)
	if err != nil {
		return fmt.Errorf("wal: read %s: %w", tail.path, err)
	}
	valid, last, scanErr := scan(data, tail.first, nil)
	if errors.Is(scanErr, errBadHeader) {
		// A crash between creating the file and writing its header. The
		// segment never held a record, so recreate it.
		slog.Warn("wal segment has a torn header, recreating", "file", filepath.Base(tail.path))
		l.segments = l.segments[:len(l.segments)-1]
		return l.newSegment(tail.first)
	}
	f, err := os.OpenFile(tail.path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("wal: open %s: %w", tail.path, err)
	}
	if scanErr != nil {
		slog.Warn("wal tail truncated", "file", filepath.Base(tail.path), "kept_bytes", valid, "dropped_bytes", len(data)-valid, "last_index", last, "cause", scanErr)
		if err := f.Truncate(int64(valid)); err != nil {
			f.Close()
			return fmt.Errorf("wal: truncate %s: %w", tail.path, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("wal: sync %s: %w", tail.path, err)
		}
	}
	if _, err := f.Seek(int64(valid), 0); err != nil {
		f.Close()
		return fmt.Errorf("wal: seek %s: %w", tail.path, err)
	}
	l.f = f
	l.last = last
	return nil
}

var errBadHeader = errors.New("bad segment header")

// scan walks the records of one segment. It returns the number of leading
// bytes that are valid, the index of the last valid record (first-1 if there
// is none) and the reason it stopped early, if it did.
func scan(data []byte, first uint64, fn func(index uint64, payload []byte) error) (valid int, last uint64, err error) {
	le := binary.LittleEndian
	last = first - 1
	if len(data) < headerSize || string(data[:8]) != segmentMagic || le.Uint64(data[8:]) != first {
		return 0, last, errBadHeader
	}
	off := headerSize
	for off < len(data) {
		rest := data[off:]
		if len(rest) < recordHeader {
			return off, last, fmt.Errorf("record header cut short at offset %d", off)
		}
		n := int(le.Uint32(rest))
		if n > maxPayload {
			return off, last, fmt.Errorf("implausible record length %d at offset %d", n, off)
		}
		if len(rest) < recordHeader+n {
			return off, last, fmt.Errorf("record cut short at offset %d", off)
		}
		body := rest[8 : recordHeader+n]
		if crc32.Checksum(body, castagnoli) != le.Uint32(rest[4:]) {
			return off, last, fmt.Errorf("checksum mismatch at offset %d", off)
		}
		index := le.Uint64(body)
		if index != last+1 {
			return off, last, fmt.Errorf("index %d follows %d at offset %d", index, last, off)
		}
		if fn != nil {
			if err := fn(index, body[8:]); err != nil {
				return off, last, err
			}
		}
		last = index
		off += recordHeader + n
	}
	return off, last, nil
}

func (l *Log) newSegment(first uint64) error {
	path := filepath.Join(l.dir, segmentName(first))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wal: create segment: %w", err)
	}
	hdr := make([]byte, headerSize)
	copy(hdr, segmentMagic)
	binary.LittleEndian.PutUint64(hdr[8:], first)
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		return fmt.Errorf("wal: write segment header: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("wal: sync segment header: %w", err)
	}
	if err := syncDir(l.dir); err != nil {
		f.Close()
		return err
	}
	l.f = f
	l.last = first - 1
	l.segments = append(l.segments, segment{first: first, path: path})
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("wal: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("wal: sync dir: %w", err)
	}
	return nil
}

// LastIndex is the index of the newest record, appended or recovered.
func (l *Log) LastIndex() uint64 { return l.last }

// Written and Synced report how far the log has been handed to the OS and
// forced to stable storage. Both are safe to call from any goroutine.
func (l *Log) Written() uint64 { return l.written.Load() }
func (l *Log) Synced() uint64  { return l.synced.Load() }

// Append buffers one record and returns its index. Nothing reaches the file
// until Commit.
func (l *Log) Append(payload []byte) (uint64, error) {
	if len(payload) > maxPayload {
		return 0, fmt.Errorf("wal: payload of %d bytes exceeds the %d byte limit", len(payload), maxPayload)
	}
	l.last++
	le := binary.LittleEndian
	start := len(l.buf)
	l.buf = le.AppendUint32(l.buf, uint32(len(payload)))
	l.buf = le.AppendUint32(l.buf, 0)
	l.buf = le.AppendUint64(l.buf, l.last)
	l.buf = append(l.buf, payload...)
	le.PutUint32(l.buf[start+4:], crc32.Checksum(l.buf[start+8:], castagnoli))
	return l.last, nil
}

// Commit writes all buffered records with one write call and, under
// SyncBatch, fsyncs. After an error the log must not be used again: the
// state of the file is unknown.
func (l *Log) Commit() error {
	if p := l.syncErr.Load(); p != nil {
		return fmt.Errorf("wal: background sync failed: %w", *p)
	}
	if len(l.buf) == 0 {
		return nil
	}
	if _, err := l.f.Write(l.buf); err != nil {
		return fmt.Errorf("wal: write: %w", err)
	}
	l.buf = l.buf[:0]
	l.written.Store(l.last)
	if l.opts.Sync == SyncBatch {
		if err := l.f.Sync(); err != nil {
			return fmt.Errorf("wal: sync: %w", err)
		}
		l.advanceSynced(l.last)
	}
	return nil
}

// Sync commits and forces everything written so far to stable storage,
// whatever the policy.
func (l *Log) Sync() error {
	if err := l.Commit(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("wal: sync: %w", err)
	}
	l.advanceSynced(l.last)
	return nil
}

// advanceSynced raises the synced index, never lowers it: the background
// syncer may finish an older fsync after Sync has recorded a newer one.
func (l *Log) advanceSynced(v uint64) {
	for {
		cur := l.synced.Load()
		if v <= cur || l.synced.CompareAndSwap(cur, v) {
			return
		}
	}
}

func (l *Log) syncLoop() {
	defer close(l.syncDone)
	t := time.NewTicker(l.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		if l.written.Load() == l.synced.Load() {
			continue
		}
		l.mu.Lock()
		// Anything written before this load is covered by the fsync below;
		// later writes are simply picked up by the next tick.
		upTo := l.written.Load()
		err := l.f.Sync()
		l.mu.Unlock()
		if err != nil {
			l.syncErr.Store(&err)
			return
		}
		l.advanceSynced(upTo)
	}
}

// Rotate makes everything so far durable and starts a new segment, so that
// older segments can later be removed whole.
func (l *Log) Rotate() error {
	if err := l.Sync(); err != nil {
		return err
	}
	if size, err := l.f.Seek(0, 1); err == nil && size == headerSize {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("wal: close segment: %w", err)
	}
	return l.newSegment(l.last + 1)
}

// SkipTo discards the log's contents and restarts it after index. It is for
// the case where a snapshot is newer than anything the log still holds.
func (l *Log) SkipTo(index uint64) error {
	if index <= l.last {
		return fmt.Errorf("wal: cannot skip back from %d to %d", l.last, index)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("wal: close segment: %w", err)
	}
	for _, s := range l.segments {
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("wal: remove segment: %w", err)
		}
	}
	l.segments = nil
	l.buf = l.buf[:0]
	if err := l.newSegment(index + 1); err != nil {
		return err
	}
	l.written.Store(index)
	l.synced.Store(index)
	return nil
}

// Replay calls fn for every committed record with an index greater than
// after, in order. It fails with ErrCorrupt if the log does not hold a
// contiguous run from after+1 to LastIndex.
func (l *Log) Replay(after uint64, fn func(index uint64, payload []byte) error) error {
	next := after + 1
	for i, s := range l.segments {
		if i+1 < len(l.segments) && l.segments[i+1].first <= next {
			continue
		}
		if s.first > next {
			return fmt.Errorf("%w: records %d..%d are missing", ErrCorrupt, next, s.first-1)
		}
		data, err := os.ReadFile(s.path)
		if err != nil {
			return fmt.Errorf("wal: read %s: %w", s.path, err)
		}
		var fnErr error
		_, last, err := scan(data, s.first, func(index uint64, payload []byte) error {
			if index < next {
				return nil
			}
			fnErr = fn(index, payload)
			return fnErr
		})
		if fnErr != nil {
			return fnErr
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCorrupt, filepath.Base(s.path), err)
		}
		if last+1 > next {
			next = last + 1
		}
	}
	if next != l.last+1 {
		return fmt.Errorf("%w: replay ended at %d, log ends at %d", ErrCorrupt, next-1, l.last)
	}
	return nil
}

// RemoveThrough deletes every segment whose records all have an index <=
// index. The current segment is never removed.
func (l *Log) RemoveThrough(index uint64) error {
	keep := 0
	for keep+1 < len(l.segments) && l.segments[keep+1].first <= index+1 {
		if err := os.Remove(l.segments[keep].path); err != nil {
			return fmt.Errorf("wal: remove segment: %w", err)
		}
		keep++
	}
	l.segments = l.segments[keep:]
	return nil
}

// Close commits, fsyncs and closes the log.
func (l *Log) Close() error {
	l.stopSyncer()
	err := l.Sync()
	if cerr := l.f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("wal: close: %w", cerr)
	}
	return err
}

// Abandon closes the file handle without committing buffered records or
// syncing, which is what a crash does.
func (l *Log) Abandon() {
	l.stopSyncer()
	l.f.Close()
}

func (l *Log) stopSyncer() {
	if l.stop != nil {
		close(l.stop)
		<-l.syncDone
		l.stop = nil
	}
}
