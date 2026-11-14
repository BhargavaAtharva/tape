package engine

import (
	"context"
	"sync/atomic"
	"testing"

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

