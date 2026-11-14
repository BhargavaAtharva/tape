package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/wal"
)

const testIDBase = 1 << idShift

// genCommands produces a reproducible order flow: mostly limit orders in a
// narrow band around 1000 so that levels queue and cross constantly, with
// every order type, cancels, amends and some commands that will be rejected.
func genCommands(seed uint64, n int) []book.Command {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	cmds := make([]book.Command, 0, n)
	var issued uint64
	target := func() uint64 { return testIDBase + 1 + rng.Uint64N(issued+1) }
	for range n {
		trader := 1 + rng.Uint64N(8)
		side := book.Buy
		if rng.IntN(2) == 0 {
			side = book.Sell
		}
		switch r := rng.IntN(100); {
		case r < 62:
			c := book.Command{Kind: book.CmdNew, Trader: trader, Side: side, Type: book.Limit, TIF: book.GTC,
				Price: 990 + rng.Int64N(21), Qty: 1 + rng.Int64N(50)}
			switch rng.IntN(12) {
			case 0:
				c.TIF = book.IOC
			case 1:
				c.TIF = book.FOK
			case 2:
				c.PostOnly = true
			case 3:
				c.Type, c.TIF, c.Price = book.Market, book.IOC, 0
			}
			issued++
			cmds = append(cmds, c)
		case r < 87:
			cmds = append(cmds, book.Command{Kind: book.CmdCancel, Trader: trader, OrderID: target()})
		default:
			c := book.Command{Kind: book.CmdAmend, Trader: trader, OrderID: target(), Qty: rng.Int64N(40)}
			if rng.IntN(2) == 0 {
				c.Price = 990 + rng.Int64N(21)
			}
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// counterClock makes timestamps a function of the command's position, which
// is what lets a recovered engine continue with identical events.
func counterClock(start int64) func() int64 {
	t := start
	return func() int64 { t++; return t }
}

// run feeds cmds through an engine and returns its events and final hash.
func run(t *testing.T, cfg Config, cmds []book.Command) ([]book.Event, [32]byte) {
	t.Helper()
	var sink collector
	cfg.Events = sink.add
	e, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	ctx := context.Background()
	for _, c := range cmds {
		if err := e.Enqueue(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	events, gap := sink.snapshot()
	if gap {
		t.Fatal("sequence gap")
	}
	return events, e.Book().Hash()
}

func TestSameCommandsSameEvents(t *testing.T) {
	cmds := genCommands(1, 20_000)
	cfg := Config{Symbol: "X", IDBase: testIDBase, Clock: counterClock(0)}
	ev1, h1 := run(t, cfg, cmds)
	cfg.Clock = counterClock(0)
	ev2, h2 := run(t, cfg, cmds)
	cfg.Clock = counterClock(0)
	cfg.Dir = t.TempDir()
	cfg.SnapshotEvery = 1000
	ev3, h3 := run(t, cfg, cmds)

	if !reflect.DeepEqual(ev1, ev2) || h1 != h2 {
		t.Fatal("two in-memory runs diverged")
	}
	if !reflect.DeepEqual(ev1, ev3) || h1 != h3 {
		t.Fatal("durable run diverged from in-memory run")
	}
	var fills int
	for _, ev := range ev1 {
		if ev.Kind == book.EvFill {
			fills++
		}
	}
	if fills < 1000 {
		t.Fatalf("only %d fills; the generator is not exercising matching", fills)
	}
}

func TestRestartAfterCleanClose(t *testing.T) {
	dir := t.TempDir()
	cmds := genCommands(2, 5000)
	cfg := Config{Symbol: "X", IDBase: testIDBase, Clock: counterClock(0), Dir: dir, SnapshotEvery: 700}
	_, want := run(t, cfg, cmds)

	e, err := Open(Config{Symbol: "X", IDBase: testIDBase, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	rec := e.Recovery()
	if rec.Replayed != 0 || rec.SnapshotIndex != 5000 || rec.LastIndex != 5000 {
		t.Fatalf("recovery after clean close: %+v", rec)
	}
	if e.Book().Hash() != want {
		t.Fatal("restored book differs")
	}
	snaps, _ := wal.ListSnapshots(dir)
	if len(snaps) > 2 {
		t.Fatalf("%d snapshots kept, want at most 2", len(snaps))
	}
}

func TestRecoveryFallsBackWhenNewestSnapshotIsDamaged(t *testing.T) {
	dir := t.TempDir()
	cmds := genCommands(3, 4000)
	cfg := Config{Symbol: "X", IDBase: testIDBase, Clock: counterClock(0), Dir: dir, SnapshotEvery: 1000}
	refEvents, want := run(t, cfg, cmds)

	snaps, _ := wal.ListSnapshots(dir)
	newest := filepath.Join(dir, fmt.Sprintf("snap-%020d.bin", snaps[len(snaps)-1]))
	data, _ := os.ReadFile(newest)
	data[len(data)-1] ^= 0xff
	os.WriteFile(newest, data, 0o644)

	var replayed []book.Event
	e, err := Open(Config{Symbol: "X", IDBase: testIDBase, Dir: dir, Events: func(evs []book.Event) {
		replayed = append(replayed, evs...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	rec := e.Recovery()
	if rec.SnapshotIndex != snaps[len(snaps)-2] || rec.Replayed != 4000-rec.SnapshotIndex {
		t.Fatalf("recovery %+v with snapshots %v", rec, snaps)
	}
	if e.Book().Hash() != want {
		t.Fatal("book rebuilt from the older snapshot differs")
	}
	tail := refEvents[len(refEvents)-len(replayed):]
	if !reflect.DeepEqual(replayed, tail) {
		t.Fatal("replayed events differ from the original run")
	}
}

// The crash test runs the engine in a child process and kills it with
// SIGKILL at a random point. The child streams every event to the parent
// as it is produced, so the parent knows exactly what the outside world saw
// before the crash. The parent then recovers from the same directory,
// optionally after damaging the log tail, and feeds the rest of the
// commands. What the child published, plus what recovery replays, plus what
// the recovered engine produces afterwards, must be exactly the event stream
// of an uninterrupted run, and the final books must hash the same.

const crashCommands = 20_000

func TestCrashChild(t *testing.T) {
	dir := os.Getenv("TAPE_CRASH_DIR")
	if dir == "" {
		t.Skip("only runs as the child of TestCrashAtRandomPoint")
	}
	policy, err := wal.ParseSyncPolicy(os.Getenv("TAPE_CRASH_SYNC"))
	if err != nil {
		t.Fatal(err)
	}
	out := os.NewFile(3, "events")
	var buf bytes.Buffer
	e, err := Open(Config{
		Symbol: "X", IDBase: testIDBase, Clock: counterClock(0), Dir: dir, SnapshotEvery: 1500,
		WAL: wal.Options{Sync: policy},
		Events: func(evs []book.Event) {
			buf.Reset()
			binary.Write(&buf, binary.LittleEndian, evs)
			// One unbuffered write per batch: whatever reached the pipe
			// before the kill is what subscribers would have seen.
			if _, err := out.Write(buf.Bytes()); err != nil {
				panic(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	for _, c := range genCommands(7, crashCommands) {
		if err := e.Enqueue(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	select {}
}

type damage int

const (
	intact damage = iota
	garbageTail
	tornTail
)

func TestCrashAtRandomPoint(t *testing.T) {
	if os.Getenv("TAPE_CRASH_DIR") != "" {
		t.Skip()
	}
	cmds := genCommands(7, crashCommands)
	refEvents, refHash := run(t, Config{Symbol: "X", IDBase: testIDBase, Clock: counterClock(0)}, cmds)

	trials := 9
	if testing.Short() {
		trials = 3
	}
	rng := rand.New(rand.NewPCG(uint64(os.Getpid()), 99))
	for trial := range trials {
		policy := []string{"batch", "interval", "none"}[trial%3]
		dmg := damage(trial / 3 % 3)
		killAt := 1 + rng.IntN(len(refEvents)-1)
		t.Run(fmt.Sprintf("%s/damage%d/kill@%d", policy, dmg, killAt), func(t *testing.T) {
			dir := t.TempDir()
			published := crashChild(t, dir, policy, killAt)
			if dmg != intact {
				damageTail(t, dir, dmg, rng)
			}

			var recovered []book.Event
			e, err := Open(Config{Symbol: "X", IDBase: testIDBase, Dir: dir, Events: func(evs []book.Event) {
				recovered = append(recovered, evs...)
			}})
			if err != nil {
				t.Fatal(err)
			}
			rec := e.Recovery()
			e.cfg.Clock = counterClock(int64(rec.LastIndex))
			e.Start()
			for _, c := range cmds[rec.LastIndex:] {
				if err := e.Enqueue(context.Background(), c); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			stream := append([]book.Event(nil), published...)
			for _, ev := range recovered {
				switch {
				case ev.Seq <= uint64(len(stream)):
					if stream[ev.Seq-1] != ev {
						t.Fatalf("seq %d: recovered engine re-emitted %+v, child had published %+v", ev.Seq, ev, stream[ev.Seq-1])
					}
				case ev.Seq == uint64(len(stream))+1:
					stream = append(stream, ev)
				default:
					t.Fatalf("gap: seq %d after %d", ev.Seq, len(stream))
				}
			}
			if !reflect.DeepEqual(stream, refEvents) {
				t.Fatalf("event stream differs from the uninterrupted run (%d vs %d events)", len(stream), len(refEvents))
			}
			if e.Book().Hash() != refHash {
				t.Fatal("final book hash differs from the uninterrupted run")
			}
			t.Logf("published %d events before kill; snapshot %d, replayed %d commands in %v",
				len(published), rec.SnapshotIndex, rec.Replayed, rec.Duration)
		})
	}
}

// crashChild starts the child, reads killAt events, kills it, and returns
// every event it managed to publish.
func crashChild(t *testing.T, dir, policy string, killAt int) []book.Event {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
	cmd.Env = append(os.Environ(), "TAPE_CRASH_DIR="+dir, "TAPE_CRASH_SYNC="+policy)
	cmd.ExtraFiles = []*os.File{w}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()

	in := bufio.NewReader(r)
	var events []book.Event
	read := func() bool {
		var ev book.Event
		if err := binary.Read(in, binary.LittleEndian, &ev); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("reading child events: %v", err)
			}
			return false
		}
		events = append(events, ev)
		return true
	}
	for len(events) < killAt && read() {
	}
	cmd.Process.Kill()
	for read() {
	}
	cmd.Wait()
	if len(events) < killAt {
		t.Fatalf("child exited after %d events: %s", len(events), strconv.Quote(cmd.ProcessState.String()))
	}
	return events
}

func damageTail(t *testing.T, dir string, d damage, rng *rand.Rand) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if len(matches) == 0 {
		t.Fatal("no log segments")
	}
	tail := matches[len(matches)-1]
	f, err := os.OpenFile(tail, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	switch d {
	case garbageTail:
		junk := make([]byte, 1+rng.IntN(200))
		for i := range junk {
			junk[i] = byte(rng.Uint32())
		}
		f.WriteAt(junk, st.Size())
	case tornTail:
		f.Truncate(max(0, st.Size()-int64(1+rng.IntN(40))))
	}
}
