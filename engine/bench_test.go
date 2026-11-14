package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/wal"
)

// benchEngine measures the engine pipeline (channel hand-off, batching,
// optional WAL, matching, reply) with many concurrent submitters.
func benchEngine(b *testing.B, cfg Config) {
	cmds := genCommands(11, 1<<16)
	e, err := Open(cfg)
	if err != nil {
		b.Fatal(err)
	}
	e.Start()
	defer e.Close()
	var next atomic.Uint64
	b.ReportAllocs()
	b.SetParallelism(8)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			c := cmds[next.Add(1)&(1<<16-1)]
			if _, err := e.Submit(ctx, c); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "orders/s")
}

func BenchmarkSubmitInMemory(b *testing.B) {
	benchEngine(b, Config{Symbol: "X", IDBase: testIDBase})
}

func BenchmarkSubmitWALNoSync(b *testing.B) {
	benchEngine(b, Config{Symbol: "X", IDBase: testIDBase, Dir: b.TempDir(), WAL: wal.Options{Sync: wal.SyncNone}})
}

// BenchmarkRecoverReplay measures recovery with no snapshot: opening an
// engine over a log of 100k commands and replaying all of them.
func BenchmarkRecoverReplay(b *testing.B) {
	const n = 100_000
	dir := b.TempDir()
	log, err := wal.Open(dir, wal.Options{Sync: wal.SyncNone})
	if err != nil {
		b.Fatal(err)
	}
	for i, c := range genCommands(5, n) {
		c.Time = int64(i + 1)
		if _, err := log.Append(book.AppendCommand(nil, c)); err != nil {
			b.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		e, err := Open(Config{Symbol: "X", IDBase: testIDBase, Dir: dir})
		if err != nil {
			b.Fatal(err)
		}
		if r := e.Recovery(); r.Replayed != n {
			b.Fatalf("replayed %d", r.Replayed)
		}
		b.StopTimer()
		e.log.Abandon()
		b.StartTimer()
	}
	b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "cmds/s")
}
