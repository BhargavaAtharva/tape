import { useEffect, useState } from "react";
import { levelKey, type FeedStatus, type MarketView } from "../lib/feed";
import { formatPrice, formatQty } from "../lib/format";
import type { Side } from "../lib/types";
import { Empty, Panel } from "./Panel";
import styles from "./OrderBook.module.css";

interface Props {
  view: MarketView;
  scale: number;
  mine: Set<string>;
  depth: number;
  onPick: (price: number, side: Side) => void;
}

interface Row {
  price: number;
  qty: number;
  total: number;
}

const FLASH_MS = 700;
const ROW = 20;
const CHROME = 22 + 26;

function ladder(levels: Map<number, number>, side: Side, depth: number): Row[] {
  const prices = [...levels.keys()].sort((a, b) => (side === "buy" ? b - a : a - b)).slice(0, depth);
  let total = 0;
  return prices.map((price) => {
    const qty = levels.get(price)!;
    total += qty;
    return { price, qty, total };
  });
}

export function statusLabel(s: FeedStatus): string {
  return { connecting: "connecting", live: "live", resyncing: "resyncing", offline: "offline" }[s];
}

// useFit reports how many whole rows fit on each side of the ladder, so the
// book never shows a half-clipped level.
function useFit(maxRows: number) {
  // A callback ref, because the ladder only mounts once a snapshot arrives.
  const [el, setEl] = useState<HTMLDivElement | null>(null);
  const [rows, setRows] = useState(maxRows);
  useEffect(() => {
    if (!el) return;
    const fit = (h: number) => setRows(Math.max(3, Math.min(maxRows, Math.floor((h - CHROME) / 2 / ROW))));
    fit(el.clientHeight);
    const ro = new ResizeObserver(([e]) => e && fit(e.contentRect.height));
    ro.observe(el);
    return () => ro.disconnect();
  }, [el, maxRows]);
  return [setEl, rows] as const;
}

export function OrderBook({ view, scale, mine, depth, onPick }: Props) {
  const [fitRef, rows] = useFit(depth);
  const asks = ladder(view.asks, "sell", rows);
  const bids = ladder(view.bids, "buy", rows);
  const max = Math.max(asks.at(-1)?.total ?? 0, bids.at(-1)?.total ?? 0, 1);
  const bestBid = bids[0]?.price;
  const bestAsk = asks[0]?.price;
  const now = performance.now();

  const row = (r: Row, side: Side) => {
    const key = levelKey(side, r.price);
    const changed = view.changed.get(key);
    return (
      <button
        type="button"
        key={r.price}
        className={`${styles.row} ${side === "buy" ? styles.bidRow : styles.askRow}`}
        onClick={() => onPick(r.price, side)}
        title="Use this price"
      >
        <span className={styles.bar} style={{ width: `${(r.total / max) * 100}%` }} />
        {changed !== undefined && now - changed < FLASH_MS && <span key={changed} className={styles.flash} />}
        <span className={`num ${styles.price}`}>
          {mine.has(key) && <span className={styles.mine} aria-label="your order" />}
          {formatPrice(r.price, scale)}
        </span>
        <span className={`num ${styles.qty}`}>{formatQty(r.qty)}</span>
        <span className={`num ${styles.total}`}>{formatQty(r.total)}</span>
      </button>
    );
  };

  const resyncing = view.status === "resyncing";
  const extra = (
    <>
      {resyncing && <span className={styles.resync}>resyncing</span>}
      <span className="num">#{view.seq}</span>
    </>
  );

  if (view.status !== "live" && view.bids.size === 0 && view.asks.size === 0) {
    return (
      <Panel title="Order book" extra={extra} className={styles.panel}>
        <Empty>{view.status === "offline" ? "Feed offline. Reconnecting." : "Waiting for snapshot"}</Empty>
      </Panel>
    );
  }

  const spread = bestBid !== undefined && bestAsk !== undefined ? bestAsk - bestBid : undefined;
  return (
    <Panel title="Order book" extra={extra} className={styles.panel}>
      <div ref={fitRef} className={styles.ladder}>
        <div className={styles.cols}>
          <span>Price</span>
          <span>Size</span>
          <span>Total</span>
        </div>
        <div className={`${styles.side} ${styles.asks}`}>{[...asks].reverse().map((r) => row(r, "sell"))}</div>
        <div className={styles.spread}>
          <span className="label">Spread</span>
          <span className="num">{spread === undefined ? "–" : formatPrice(spread, scale)}</span>
          <span className="num muted">{spread !== undefined && bestBid ? `${((spread / bestBid) * 10_000).toFixed(1)} bp` : ""}</span>
        </div>
        <div className={`${styles.side} ${styles.bids}`}>{bids.map((r) => row(r, "buy"))}</div>
      </div>
    </Panel>
  );
}
