# tape

A limit order book matching engine in Go: price-time priority, deterministic, crash-recoverable from a write-ahead
log, with a WebSocket market data feed and a trading screen.

## Why this exists

Every exchange, from NSE to a crypto venue to a prediction market, sits on the same core: a matching engine that
has to be fast, strictly fair, deterministic, and able to rebuild its exact state after a crash. Most toy order
books get one of those right. tape tries to get all four and to prove each with a test or a measurement rather than
a claim: property and fuzz tests for the matching rules, a crash test that SIGKILLs a real process at a random point
and checks the recovered event stream byte for byte, and benchmarks with before and after numbers.

## Architecture

```
 HTTP / WS clients                                        bot traders (sim, demo mode)
        │                                                          │
        ▼                                                          ▼
  api.Server ── POST/PATCH/DELETE ──> per-symbol inbound channel (bounded) <──┘
        ▲                                     │
        │                                     ▼
        │                     engine goroutine (one per symbol, owns its book)
        │                       1. stamp time, append batch to WAL, one write + fsync (group commit)
        │                       2. apply each command to book.Book (no locks)
        │                       3. reply to submitters, hand the batch's events to the feed
        │                       4. every N commands: encode snapshot, rotate WAL, write file in background
        │                                     │ Publish (never blocks)
        │                                     ▼
        └──── GET book/trades/symbols ── feed.Feed (L2 mirror, trades ring, ticker)
                                              │ per-subscriber bounded buffer
                                              ▼
                                      WebSocket writers ──> browsers (web/)
```

Packages:

| package | what it does |
| --- | --- |
| `book` | Single-symbol order book and matching. Pure state machine, no goroutines, no clock. Command codec and snapshot encoding. |
| `wal` | Segmented append-only log with CRC-32C records, group commit, three fsync policies, atomic snapshot files. |
| `engine` | One goroutine per symbol, batching, durability, recovery, latency histogram, the router (`Exchange`). |
| `feed` | Market data: L2 mirror, sequence-chained updates, trades tape, slow-consumer and gap handling. |
| `api` | REST and WebSocket gateway, CORS, per-address rate limiting, stats sampler. |
| `sim` | Bot traders: market makers, depth provider, noise and momentum takers around a mean-reverting fair price. |
| `hist` | Lock-free log-linear latency histogram. |
| `cmd/tape` | The server. `cmd/loadgen`: the load generator. `web/`: React + TypeScript trading screen. |

## Design decisions

**One goroutine per symbol owns the book.** Matching takes no locks and the order in which the engine dequeues
commands is the single total order everything else derives from. Symbols scale across cores independently. The
cost is that one hot symbol is bounded by one core; for an exchange that is the usual trade (and the measured core
does about 17M commands/s, far beyond what the gateway can feed it).

**Price levels: a treap threaded with a sorted doubly linked list.** The treap gives O(log n) insert/remove of a
level; the list gives O(1) best price and O(1) walk to the next level while sweeping. Treap priorities are a hash of
the price rather than random draws, so the tree shape depends only on the set of live prices: there is no RNG
state that a snapshot would have to capture for recovery to be exact. Each level is a FIFO doubly linked list of
orders, and an id-to-order map gives O(1) cancel. Two heaps plus a map were the alternative; they make removal of an
arbitrary emptied level awkward (lazy deletion), which matters because cancels empty levels all the time.

**Integers everywhere.** Prices are int64 ticks and quantities int64 lots, capped at 2^40 so a level's aggregate
cannot overflow. Order ids encode the symbol (`(index+1) << 40 | counter`) so cancels route without a lookup, and
stay below 2^53 so they survive JavaScript.

**Rules.** Limit, market (must be IOC or FOK), IOC, FOK, post-only (rejected if it would cross). Amend: reducing
quantity at the same price keeps queue priority; any price change or quantity increase re-enters the order at the
back, and if the new price crosses it trades as a taker, as on real venues. Self-trade prevention is cancel-newest:
matching stops at the first resting order of the same trader, earlier fills stand and the rest of the incoming order
is cancelled. FOK checks fillability order by order, not by level totals, because STP could otherwise leave it half
filled.

**The book never reads a clock.** The engine stamps each command's time when it dequeues it, and that timestamp is
written to the log with the command. Replay therefore regenerates events identical down to the timestamps.

**WAL before apply, with group commit.** The engine drains whatever is queued (up to 256 commands), appends them
all, issues one `write` and, under `batch`, one fsync, and only then applies them. Nothing anyone sees, including
the submitter's reply, exists before it is in the log. If the log fails the engine stops (fail-stop) rather than
let the book run ahead of what recovery can rebuild. Three policies:

- `batch`: fsync every group commit. Nothing acknowledged is ever lost. Cost on this Mac: see Results.
- `interval`: a background fsync every 10 ms. A process crash loses nothing (the data is in the page cache); a power
  failure can lose one interval.
- `none`: flushing left to the OS.

The demo container uses `interval`: the platform gives no durable disk anyway, and it keeps order latency low.

**Records and torn tails.** A record is `len | CRC-32C | index | payload`; the checksum covers the index, so a
record from the wrong place is detected too. On open, the newest segment is scanned and cut back to the last valid
record: a torn write, a garbage tail or a corrupt last record are all truncated cleanly, and a torn segment header
is recreated. Damage in a sealed, older segment is not explainable by a crash, so replay refuses with `ErrCorrupt`
instead of silently skipping commands. Trade-off: a bad record in the middle of the newest segment is treated like
a torn tail and everything after it is dropped; with fsync-per-batch that can only happen to the last batch.

**Snapshots.** Every 50k commands the engine encodes the book on its own goroutine (where it is consistent), rotates
the log so the snapshot boundary is a segment boundary, and writes the file from a helper goroutine (temp file,
fsync, rename, fsync dir) so a large book does not stall matching. Once it is durable the engine deletes log
segments and snapshots older than the *previous* snapshot: if the newest snapshot is ever damaged, recovery falls
back to the older one plus a longer replay (this is tested). A clean shutdown writes a final snapshot so restarts
replay nothing. The snapshot encoding is canonical (best price first, oldest first), so its SHA-256 is the book
hash used by the determinism tests.

**Market data is decoupled from matching.** The engine hands each batch to `feed.Publish`, which never blocks: if
the feed's inbound buffer is full the batch is dropped and counted. The feed detects the resulting sequence gap
itself, rebuilds its L2 mirror from the engine (a `Query` that runs between batches, so the state is exactly at a
batch boundary), drops its subscribers and carries on. Each WebSocket subscriber has its own bounded buffer; a full
buffer or a write that takes longer than 5 s drops that client with close code 1008, and nobody else notices.
Updates carry `seq` and `prev` (the previous message's seq); a client that sees `prev` differ from its last seq
reconnects for a fresh snapshot. Snapshot and registration happen under the same lock as applying updates, so a
subscriber's first update always chains onto its snapshot.

**Allocation work.** The book recycles `Order` and level structs through free lists and reuses its event buffer, so
in steady state it allocates nothing per command (2 allocs/op before). Consequence: `Book.Apply`'s result is valid
only until the next call and `Book.Order` pointers only until the next `Apply`; the engine copies events into one
per-batch slice. Reply channels come from a `sync.Pool`.

**Public demo safety.** Trader identity is a header (`X-Trader-ID`), not authentication: accounts are out of scope.
Ids below 1000 belong to the bots and are refused at the gateway. Order entry is rate limited per client address
(token bucket, 20/s, burst 40), bodies are capped at 4 KB.

**Cut, deliberately.** Accounts, balances, auctions and replication are out of scope (PRD). The trades tape held by
the feed is in memory: after a restart from a snapshot it starts empty, because replay only regenerates events after
the snapshot.

## Running locally

Requires Go 1.27 and Node 25.

```sh
go run ./cmd/tape                       # :8081, WAL in ./data, bots on
cd web && npm install && npm run dev    # http://localhost:5174, proxies /v1 to :8081
```

Configuration is environment-only; see `.env.example`. Useful ones: `TAPE_WAL_SYNC=batch|interval|none`,
`TAPE_DATA_DIR=` (empty for in-memory), `TAPE_SIM=false`, `TAPE_RATE_LIMIT=0` (needed for load tests).

API:

```
GET    /v1/symbols                    ticker per symbol
GET    /v1/book/{symbol}?depth=20     L2 book
GET    /v1/trades/{symbol}?limit=100  recent trades, oldest first
GET    /v1/stats                      orders/s, latency p50/p99/p99.9, WAL written/synced/lag, feed drops
GET    /v1/orders?symbol=S            open orders of X-Trader-ID
POST   /v1/orders                     {"symbol","side","type","tif","post_only","price","qty"}
PATCH  /v1/orders/{id}                {"price","qty"}  (0 = unchanged)
DELETE /v1/orders/{id}
GET    /v1/ws/{symbol}?depth=50       snapshot, then {"type":"update","seq","prev","deltas","trades"}
```

## Tests

```sh
go test -race ./...                                    # everything, including the crash test
go test -run '^$' -fuzz FuzzBook -fuzztime=60s ./book  # fuzz the book
go test -run '^$' -bench . -benchtime=2s ./book ./engine
cd web && npm run build && node scripts/smoke-ws.mjs http://localhost:8081 6
```

What they check:

- `book`: table-driven tests per rule; randomized streams and a fuzz target that, after every command, check the
  book is never crossed, quantity is conserved (submitted = filled + resting + cancelled per order), sequence numbers
  strictly increase, fills respect limit prices and price-time priority (against an independent ledger rebuilt from
  events), and that a clone restored from a mid-stream snapshot stays event-for-event identical.
- `wal`: every possible torn cut of the last record, bit flips, absurd lengths, torn segment headers, rotation and
  pruning, damage in sealed segments, snapshot fallback.
- `engine`: the same 20k-command stream run twice in memory and once with the WAL gives identical events and hash;
  `TestCrashAtRandomPoint` re-executes the test binary as a child that runs a durable engine and streams every event
  to the parent through a pipe; the parent SIGKILLs it at a random event, optionally appends garbage to or truncates
  the log tail, recovers in-process, feeds the remaining commands and requires the union of published, replayed and
  new events to equal the uninterrupted run exactly, with the same final book hash. Nine trials across all three
  sync policies and three damage modes.
- `api`: a real HTTP server with real WebSocket clients: one with a 1 KB socket receive buffer that never reads is
  dropped with a policy-violation close while three normal clients get an unbroken sequence and end with exactly
  the server's book.

## Results

Apple M4, 16 GB, macOS, Go 1.27.4. Other builds were running on the machine at the same time, so treat small
differences as noise. Full outputs and commands are in `VERIFICATION.md`.

### Matching core (`go test -run '^$' -bench . -benchtime=2s -count=5 ./book`)

Median of 5 runs. "Before" is commit `944b375` (prior to the allocation work) with the same benchmark file.

| benchmark | before ns/op | before allocs | after ns/op | after allocs | after orders/s |
| --- | --- | --- | --- | --- | --- |
| ApplyMixed (60% limit, 10% market, 25% cancel, 5% amend) | 103.4 | 2 | 58.6 | 0 | 17.1M |
| RestAndCancel (100k rests then 100k cancels) | 97.7 | 2 | 70.7 | 0 | 14.2M |
| Sweep (taker crosses 5 refilled levels) | 118.5 | 4 | 64.9 | 0 | 15.4M |

### Engine pipeline (`./engine`, 8 concurrent submitters, one symbol)

| benchmark | before ns/op | before allocs | after ns/op | after allocs |
| --- | --- | --- | --- | --- |
| SubmitInMemory | 934.7 | 4 | 970.9 | 0 |
| SubmitWALNoSync | 1299 | 4 | 1214 | 0 |

The allocations are gone but the time is dominated by the goroutine hand-off and is within run-to-run noise; I do
not claim a speed-up here.

Recovery: replaying 100k logged commands with no snapshot (`BenchmarkRecoverReplay`) takes a median 12.5 ms
(about 8.0M commands/s).

### Full stack under load (`cmd/loadgen`, 4 symbols, closed loop)

In-process, WAL on, latency from `Submit` to result:

| sync | workers/symbol | throughput | p50 | p99 | p99.9 |
| --- | --- | --- | --- | --- | --- |
| batch | 16 | 2,929/s | 23.1 ms | 35.7 ms | 44.0 ms |
| batch | 256 | 49,795/s | 18.9 ms | 41.9 ms | 71.3 ms |
| interval | 16 | 7,708/s | 10.5 ms | 22.0 ms | 30.4 ms |
| none | 16 | 662,770/s | 53 µs | 311 µs | 5.8 ms |

Over HTTP against `./bin/tape` (`TAPE_SIM=false TAPE_RATE_LIMIT=0`), 32 keep-alive clients per symbol:

| sync | throughput | p50 | p99 | p99.9 |
| --- | --- | --- | --- | --- |
| batch | 4,967/s | 24.1 ms | 56.6 ms | 75.5 ms |
| interval | 13,297/s | 7.1 ms | 50.3 ms | 65.0 ms |
| none | 86,586/s | 1.18 ms | 6.29 ms | 11.5 ms |

Reading these honestly: with durability on, this Mac is bound by fsync. Go's `File.Sync` on macOS issues
`F_FULLFSYNC`, which flushes the drive's write cache and takes on the order of 20 ms here; group commit is what
turns that into thousands of orders per second (compare 16 vs 256 workers: the same fsync rate, sixteen times the
batch size). `interval` is slower than it should be on macOS because writes to the file wait behind the in-flight
full fsync. Without fsync the HTTP gateway, not the engine, is the limit. Latency percentiles come from a log-linear
histogram with about 6% bucket error.

## Deployment

- Backend: `Dockerfile` (static binary on distroless, uid 1000, port 7860, WAL in `/tmp`). Space metadata in
  `deploy/hf-space/README.md`.
- Frontend: `web/` is a static Vite build; set `VITE_API_URL` and `VITE_WS_URL` at build time.
