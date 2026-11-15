// Connects to a running tape server and checks the market data feed: every
// symbol must send a snapshot followed by updates whose sequence numbers
// chain without gaps, the locally maintained book must never cross, and at
// a common sequence number it must equal the snapshot a second client gets.
//
// Usage: node scripts/smoke-ws.mjs [base-url] [seconds]

const base = (process.argv[2] ?? process.env.TAPE_URL ?? "http://localhost:8081").replace(/\/$/, "");
const seconds = Number(process.argv[3] ?? 5);
const wsBase = base.replace(/^http/, "ws");

function open(symbol) {
  return new WebSocket(`${wsBase}/v1/ws/${symbol}`);
}

function nextMessage(ws) {
  return new Promise((resolve, reject) => {
    ws.addEventListener("message", (e) => resolve(JSON.parse(e.data)), { once: true });
    ws.addEventListener("close", (e) => reject(new Error(`closed ${e.code} ${e.reason}`)), { once: true });
  });
}

function fingerprint(bids, asks) {
  return [...[...bids].map(([p, q]) => `b${p}:${q}`), ...[...asks].map(([p, q]) => `a${p}:${q}`)].sort().join(",");
}

async function check(symbol) {
  const ws = open(symbol);
  const book = { seq: 0, bids: new Map(), asks: new Map() };
  let updates = 0;
  let trades = 0;
  let resyncs = 0;
  let target = null;
  let matched = false;
  // Fingerprints of recent states by sequence number: the second client's
  // snapshot may name a sequence number this stream has already passed.
  const seen = new Map();
  const compare = () => {
    const fp = seen.get(target.seq);
    if (fp === undefined) return;
    if (fp === fingerprint(new Map(target.bids), new Map(target.asks))) matched = true;
    else failure ??= `book differs from a fresh snapshot at seq ${target.seq}`;
  };
  let failure = null;

  const done = new Promise((resolve) => {
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.type === "snapshot") {
        // A later snapshot is the server resyncing this client in place.
        if (book.seq !== 0) resyncs++;
        book.seq = m.seq;
        book.bids = new Map(m.bids);
        book.asks = new Map(m.asks);
        seen.set(m.seq, fingerprint(book.bids, book.asks));
        return;
      }
      if (m.prev !== book.seq) failure ??= `gap: update prev=${m.prev} after seq=${book.seq}`;
      if (m.seq <= m.prev) failure ??= `non-increasing seq ${m.prev} -> ${m.seq}`;
      book.seq = m.seq;
      updates++;
      trades += m.trades?.length ?? 0;
      for (const d of m.deltas ?? []) {
        const side = d.side === "buy" ? book.bids : book.asks;
        if (d.qty === 0) side.delete(d.price);
        else side.set(d.price, d.qty);
      }
      const bestBid = Math.max(...book.bids.keys());
      const bestAsk = Math.min(...book.asks.keys());
      if (book.bids.size && book.asks.size && bestBid >= bestAsk) failure ??= `crossed book at seq ${m.seq}: ${bestBid} >= ${bestAsk}`;
      seen.set(m.seq, fingerprint(book.bids, book.asks));
      if (seen.size > 2000) seen.delete(seen.keys().next().value);
      if (target && m.seq === target.seq) compare();
    };
    ws.onclose = (e) => {
      failure ??= `connection closed early: ${e.code} ${e.reason}`;
      resolve();
    };
    setTimeout(async () => {
      // A second client's snapshot names a sequence number this stream will
      // pass through; at that point both views must be identical.
      const second = open(symbol);
      try {
        target = await nextMessage(second);
      } catch (err) {
        failure ??= `second client: ${err.message}`;
      }
      second.close();
      if (target) compare();
      setTimeout(() => {
        ws.onclose = null;
        ws.close();
        resolve();
      }, seconds * 500);
    }, seconds * 500);
  });
  await done;
  if (!failure && !updates) failure = "no updates received";
  if (!failure && !matched) failure = "never reached the second snapshot's sequence number";
  return { symbol, updates, trades, resyncs, seq: book.seq, levels: book.bids.size + book.asks.size, failure };
}

const symbols = await fetch(`${base}/v1/symbols`).then((r) => r.json());
const results = await Promise.all(symbols.map((s) => check(s.symbol)));
let failed = false;
for (const r of results) {
  const status = r.failure ? `FAIL ${r.failure}` : "ok";
  console.log(`${r.symbol.padEnd(10)} updates=${r.updates} trades=${r.trades} resyncs=${r.resyncs} last_seq=${r.seq} levels=${r.levels} ${status}`);
  failed ||= !!r.failure;
}
process.exit(failed ? 1 : 0);
