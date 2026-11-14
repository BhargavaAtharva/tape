package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func payload(i uint64) []byte {
	return fmt.Appendf(nil, "record-%d-%s", i, bytes.Repeat([]byte{'x'}, int(i%7)))
}

func openLog(t *testing.T, dir string, opts Options) *Log {
	t.Helper()
	l, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for range n {
		want := l.LastIndex() + 1
		got, err := l.Append(payload(want))
		if err != nil || got != want {
			t.Fatalf("append gave index %d, %v; want %d", got, err, want)
		}
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
}

// collect replays the log and checks every payload against payload(index).
func collect(t *testing.T, l *Log, after uint64) []uint64 {
	t.Helper()
	var got []uint64
	err := l.Replay(after, func(i uint64, p []byte) error {
		if !bytes.Equal(p, payload(i)) {
			return fmt.Errorf("record %d holds %q", i, p)
		}
		got = append(got, i)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wantRange(t *testing.T, got []uint64, from, to uint64) {
	t.Helper()
	if from > to {
		if len(got) != 0 {
			t.Fatalf("replayed %d records, want none", len(got))
		}
		return
	}
	if uint64(len(got)) != to-from+1 || got[0] != from || got[len(got)-1] != to {
		t.Fatalf("replayed %d records %v..., want %d..%d", len(got), got[:min(len(got), 3)], from, to)
	}
}

func tailSegment(t *testing.T, dir string) string {
	t.Helper()
	segs, err := listSegments(dir)
	if err != nil || len(segs) == 0 {
		t.Fatalf("no segments: %v", err)
	}
	return segs[len(segs)-1].path
}

func TestAppendReopenReplay(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 50)
	if l.Written() != 50 || l.Synced() != 50 {
		t.Fatalf("written %d synced %d", l.Written(), l.Synced())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 50 {
		t.Fatalf("reopened at %d", l.LastIndex())
	}
	wantRange(t, collect(t, l, 0), 1, 50)
	wantRange(t, collect(t, l, 42), 43, 50)
	wantRange(t, collect(t, l, 50), 1, 0)
	appendN(t, l, 5)
	wantRange(t, collect(t, l, 48), 49, 55)
}

func TestUncommittedRecordsAreLostOnCrash(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 10)
	for range 3 {
		l.Append([]byte("never committed"))
	}
	l.Abandon()

	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 10 {
		t.Fatalf("recovered to %d, want 10", l.LastIndex())
	}
}

// Every possible torn write of the last record must be cut back to the end
// of the previous one, and the log must then accept new records.
func TestTornTailIsTruncated(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 20)
	l.Close()
	path := tailSegment(t, dir)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastLen := recordHeader + len(payload(20))
	goodPrefix := len(full) - lastLen

	for cut := goodPrefix + 1; cut < len(full); cut++ {
		if err := os.WriteFile(path, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		l := openLog(t, dir, Options{Sync: SyncBatch})
		if l.LastIndex() != 19 {
			t.Fatalf("cut at %d: recovered to %d, want 19", cut, l.LastIndex())
		}
		if st, _ := os.Stat(path); st.Size() != int64(goodPrefix) {
			t.Fatalf("cut at %d: file is %d bytes, want %d", cut, st.Size(), goodPrefix)
		}
		appendN(t, l, 1)
		wantRange(t, collect(t, l, 0), 1, 20)
		l.Close()
	}
}

func TestCorruptTailRecordIsTruncated(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 20)
	l.Close()
	path := tailSegment(t, dir)
	full, _ := os.ReadFile(path)
	lastLen := recordHeader + len(payload(20))

	for _, off := range []int{4, 8, recordHeader, lastLen - 1} {
		damaged := bytes.Clone(full)
		damaged[len(full)-lastLen+off] ^= 0x40
		os.WriteFile(path, damaged, 0o644)
		l := openLog(t, dir, Options{Sync: SyncBatch})
		if l.LastIndex() != 19 {
			t.Fatalf("flip at %d: recovered to %d, want 19", off, l.LastIndex())
		}
		l.Close()
		os.WriteFile(path, full, 0o644)
	}

	// A length field gone wild must not make Open allocate or read past EOF.
	damaged := bytes.Clone(full)
	copy(damaged[len(full)-lastLen:], []byte{0xff, 0xff, 0xff, 0x7f})
	os.WriteFile(path, damaged, 0o644)
	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 19 {
		t.Fatalf("bad length: recovered to %d, want 19", l.LastIndex())
	}
}

func TestTornSegmentHeaderIsRecreated(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 5)
	l.Close()
	torn := filepath.Join(dir, segmentName(6))
	os.WriteFile(torn, []byte("TAPE"), 0o644)

	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 5 {
		t.Fatalf("recovered to %d", l.LastIndex())
	}
	appendN(t, l, 2)
	wantRange(t, collect(t, l, 0), 1, 7)
}

func TestRotateAndRemoveThrough(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	for range 4 {
		appendN(t, l, 10)
		if err := l.Rotate(); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Rotate(); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 3)
	if segs, _ := listSegments(dir); len(segs) != 5 {
		t.Fatalf("%d segments, want 5 (rotating an empty segment is a no-op)", len(segs))
	}
	wantRange(t, collect(t, l, 15), 16, 43)

	if err := l.RemoveThrough(25); err != nil {
		t.Fatal(err)
	}
	if segs, _ := listSegments(dir); len(segs) != 3 || segs[0].first != 21 {
		t.Fatalf("after RemoveThrough(25): %+v", segs)
	}
	wantRange(t, collect(t, l, 25), 26, 43)
	if err := l.Replay(5, func(uint64, []byte) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("replay from a removed range: %v", err)
	}
	l.Close()

	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 43 {
		t.Fatalf("reopened at %d", l.LastIndex())
	}
	wantRange(t, collect(t, l, 30), 31, 43)
}

func TestDamageBeforeTheTailIsReported(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 10)
	l.Rotate()
	appendN(t, l, 10)
	l.Close()

	segs, _ := listSegments(dir)
	data, _ := os.ReadFile(segs[0].path)
	data[len(data)-2] ^= 1
	os.WriteFile(segs[0].path, data, 0o644)

	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	err := l.Replay(0, func(uint64, []byte) error { return nil })
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("replay over a damaged sealed segment: %v", err)
	}
}

func TestReplayStopsOnCallbackError(t *testing.T) {
	l := openLog(t, t.TempDir(), Options{Sync: SyncNone})
	defer l.Close()
	appendN(t, l, 10)
	stop := errors.New("stop")
	n := 0
	err := l.Replay(0, func(uint64, []byte) error {
		n++
		if n == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || n != 3 {
		t.Fatalf("replay returned %v after %d records", err, n)
	}
}

func TestSkipTo(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, Options{Sync: SyncBatch})
	appendN(t, l, 10)
	if err := l.SkipTo(5); err == nil {
		t.Fatal("skipping backwards succeeded")
	}
	if err := l.SkipTo(100); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 2)
	wantRange(t, collect(t, l, 100), 101, 102)
	l.Close()
	l = openLog(t, dir, Options{Sync: SyncBatch})
	defer l.Close()
	if l.LastIndex() != 102 {
		t.Fatalf("reopened at %d", l.LastIndex())
	}
}

func TestIntervalSyncCatchesUp(t *testing.T) {
	l := openLog(t, t.TempDir(), Options{Sync: SyncInterval, Interval: time.Millisecond})
	defer l.Close()
	appendN(t, l, 100)
	if l.Written() != 100 {
		t.Fatalf("written %d", l.Written())
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Synced() != 100 {
		if time.Now().After(deadline) {
			t.Fatalf("synced stuck at %d", l.Synced())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOversizedPayloadIsRefused(t *testing.T) {
	l := openLog(t, t.TempDir(), Options{Sync: SyncNone})
	defer l.Close()
	if _, err := l.Append(make([]byte, maxPayload+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if l.LastIndex() != 0 {
		t.Fatalf("refused append moved the index to %d", l.LastIndex())
	}
}

func TestParseSyncPolicy(t *testing.T) {
	for _, p := range []SyncPolicy{SyncBatch, SyncInterval, SyncNone} {
		got, err := ParseSyncPolicy(p.String())
		if err != nil || got != p {
			t.Fatalf("%v round-tripped to %v, %v", p, got, err)
		}
	}
	if _, err := ParseSyncPolicy("sometimes"); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestSnapshots(t *testing.T) {
	dir := t.TempDir()
	if idx, state, err := LoadSnapshot(dir); err != nil || idx != 0 || state != nil {
		t.Fatalf("empty dir: %d %q %v", idx, state, err)
	}
	for _, i := range []uint64{10, 20, 30} {
		if err := WriteSnapshot(dir, i, fmt.Appendf(nil, "state@%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, snapshotName(40)+tempSuffix), []byte("half"), 0o644)

	idx, state, err := LoadSnapshot(dir)
	if err != nil || idx != 30 || string(state) != "state@30" {
		t.Fatalf("got %d %q %v", idx, state, err)
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName(40)+tempSuffix)); !os.IsNotExist(err) {
		t.Fatal("temporary snapshot file left behind")
	}

	newest := filepath.Join(dir, snapshotName(30))
	data, _ := os.ReadFile(newest)
	data[len(data)-1] ^= 1
	os.WriteFile(newest, data, 0o644)
	idx, state, err = LoadSnapshot(dir)
	if err != nil || idx != 20 || string(state) != "state@20" {
		t.Fatalf("with newest damaged: %d %q %v", idx, state, err)
	}

	if err := RemoveSnapshot(dir, 10); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListSnapshots(dir); len(got) != 2 || got[0] != 20 {
		t.Fatalf("after remove: %v", got)
	}
}
