// Command loadgen drives tape with a closed-loop order flow and reports
// throughput and latency percentiles.
//
// In engine mode it runs the exchange in-process, with the write-ahead log
// on, and measures from Submit to result: queueing, group commit (including
// fsync under -sync=batch) and matching. In http mode it targets a running
// server and measures full request round trips over keep-alive connections.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BhargavaAtharva/tape/book"
	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/hist"
	"github.com/BhargavaAtharva/tape/wal"
)

type options struct {
	mode     string
	url      string
	symbols  []string
	workers  int
	duration time.Duration
	sync     wal.SyncPolicy
	dataDir  string
}

func main() {
	var o options
	var symbols, policy string
	flag.StringVar(&o.mode, "mode", "engine", "engine (in-process, WAL on) or http")
	flag.StringVar(&o.url, "url", "http://localhost:8081", "server base URL for http mode")
	flag.StringVar(&symbols, "symbols", "BTC-USD,ETH-USD,SOL-USD,TAPE", "symbols to trade")
	flag.IntVar(&o.workers, "workers", 16, "concurrent clients per symbol")
	flag.DurationVar(&o.duration, "duration", 10*time.Second, "how long to run")
	flag.StringVar(&policy, "sync", "batch", "WAL sync policy for engine mode: batch, interval or none")
	flag.StringVar(&o.dataDir, "data", "", "WAL directory for engine mode (default: a fresh temp dir)")
	flag.Parse()
	o.symbols = strings.Split(symbols, ",")
	var err error
	if o.sync, err = wal.ParseSyncPolicy(policy); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

// submitter sends one order command and returns the id of the order if it
// is left resting, so the worker can cancel it later.
type submitter func(ctx context.Context, symbol string, c book.Command) (resting uint64, err error)

func run(o options) error {
	var submit submitter
	var stop func() error
	switch o.mode {
	case "engine":
		dir := o.dataDir
		if dir == "" {
			tmp, err := os.MkdirTemp("", "tape-loadgen-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			dir = tmp
		}
		x, err := engine.OpenExchange(o.symbols, func(c *engine.Config) {
			c.Dir = filepath.Join(dir, c.Symbol)
			c.WAL = wal.Options{Sync: o.sync}
		})
		if err != nil {
			return err
		}
		x.Start()
		submit = engineSubmitter(x)
		stop = x.Close
		fmt.Printf("mode=engine wal_sync=%s data=%s\n", o.sync, dir)
	case "http":
		submit = httpSubmitter(o.url, o.workers*len(o.symbols))
		stop = func() error { return nil }
		fmt.Printf("mode=http url=%s\n", o.url)
	default:
		return fmt.Errorf("unknown mode %q", o.mode)
	}

	var lat hist.Histogram
	var orders, failures atomic.Uint64
	ctx, cancel := context.WithTimeout(context.Background(), o.duration)
	defer cancel()
	var wg sync.WaitGroup
	var firstErr atomic.Pointer[error]
	start := time.Now()
	for si, sym := range o.symbols {
		for w := range o.workers {
			trader := uint64(si*o.workers + w + 1)
			wg.Go(func() {
				rng := rand.New(rand.NewPCG(trader, 1))
				var resting []uint64
				for ctx.Err() == nil {
					c := nextCommand(rng, trader, &resting)
					t0 := time.Now()
					id, err := submit(ctx, sym, c)
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						failures.Add(1)
						firstErr.CompareAndSwap(nil, &err)
						continue
					}
					lat.Record(int64(time.Since(t0)))
					orders.Add(1)
					if id != 0 {
						resting = append(resting, id)
					}
				}
			})
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	if err := stop(); err != nil {
		return err
	}

	c := lat.Snapshot()
	n := orders.Load()
	fmt.Printf("symbols=%d workers_per_symbol=%d duration=%s\n", len(o.symbols), o.workers, elapsed.Round(time.Millisecond))
	fmt.Printf("commands=%d failures=%d throughput=%.0f/s\n", n, failures.Load(), float64(n)/elapsed.Seconds())
	fmt.Printf("latency p50=%s p99=%s p99.9=%s\n", dur(c.Quantile(0.5)), dur(c.Quantile(0.99)), dur(c.Quantile(0.999)))
	if p := firstErr.Load(); p != nil {
		fmt.Printf("first error: %v\n", *p)
	}
	return nil
}

func dur(ns int64) string { return time.Duration(ns).String() }

// nextCommand produces the order flow: limit orders around a fixed mid,
// some marketable, and cancels of the worker's own resting orders.
func nextCommand(rng *rand.Rand, trader uint64, resting *[]uint64) book.Command {
	if r := *resting; len(r) > 0 && rng.IntN(100) < 30 {
		k := rng.IntN(len(r))
		id := r[k]
		r[k] = r[len(r)-1]
		*resting = r[:len(r)-1]
		return book.Command{Kind: book.CmdCancel, Trader: trader, OrderID: id}
	}
	side := book.Side(1 + rng.IntN(2))
	if rng.IntN(100) < 10 {
		return book.Command{Kind: book.CmdNew, Type: book.Market, TIF: book.IOC, Side: side, Trader: trader, Qty: 1 + rng.Int64N(10)}
	}
	off := rng.Int64N(40) - 4
	price := 100_000 - off
	if side == book.Sell {
		price = 100_000 + off
	}
	return book.Command{Kind: book.CmdNew, Type: book.Limit, TIF: book.GTC, Side: side, Trader: trader, Price: price, Qty: 1 + rng.Int64N(20)}
}

func engineSubmitter(x *engine.Exchange) submitter {
	return func(ctx context.Context, symbol string, c book.Command) (uint64, error) {
		e, ok := x.Engine(symbol)
		if !ok {
			return 0, fmt.Errorf("unknown symbol %s", symbol)
		}
		res, err := e.Submit(ctx, c)
		if err != nil {
			return 0, err
		}
		return restingID(res.Events), nil
	}
}

// restingID reports the order id if the command left an order resting.
func restingID(evs []book.Event) uint64 {
	if len(evs) == 0 || evs[0].Kind != book.EvAccepted {
		return 0
	}
	open := evs[0].Qty
	for _, e := range evs[1:] {
		if e.OrderID == evs[0].OrderID && (e.Kind == book.EvFill || e.Kind == book.EvCancelled) {
			open -= e.Qty
		}
	}
	if open > 0 {
		return evs[0].OrderID
	}
	return 0
}

func httpSubmitter(base string, conns int) submitter {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{MaxIdleConns: conns, MaxIdleConnsPerHost: conns, MaxConnsPerHost: conns},
	}
	sides := map[book.Side]string{book.Buy: "buy", book.Sell: "sell"}
	return func(ctx context.Context, symbol string, c book.Command) (uint64, error) {
		var req *http.Request
		var err error
		if c.Kind == book.CmdCancel {
			req, err = http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/v1/orders/%d", base, c.OrderID), nil)
		} else {
			body := map[string]any{"symbol": symbol, "side": sides[c.Side], "qty": c.Qty}
			if c.Type == book.Market {
				body["type"] = "market"
			} else {
				body["price"] = c.Price
			}
			b, _ := json.Marshal(body)
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/orders", bytes.NewReader(b))
		}
		if err != nil {
			return 0, err
		}
		req.Header.Set("X-Trader-ID", fmt.Sprint(c.Trader))
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		var out struct {
			OrderID uint64 `json:"order_id"`
			Status  string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return 0, errors.New("rate limited: start the server with TAPE_RATE_LIMIT=0")
		case resp.StatusCode >= 500:
			return 0, fmt.Errorf("server error %d", resp.StatusCode)
		}
		if out.Status == "resting" {
			return out.OrderID, nil
		}
		return 0, nil
	}
}
