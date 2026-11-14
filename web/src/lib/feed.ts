import { useEffect, useState, useSyncExternalStore } from "react";
import { api } from "./api";
import { WS_URL } from "./config";
import type { Side, Snapshot, Trade, Update } from "./types";

export type FeedStatus = "connecting" | "live" | "resyncing" | "offline";

export interface MarketView {
  status: FeedStatus;
  seq: number;
  bids: Map<number, number>;
  asks: Map<number, number>;
  /** When each level last changed, keyed "buy:price" / "sell:price". */
  changed: Map<string, number>;
  /** Oldest first. */
  trades: Trade[];
  resyncs: number;
  version: number;
}

const MAX_TRADES = 3000;
const CLOSE_TRY_AGAIN = 1013;

/**
 * MarketFeed keeps one symbol's book and tape from the WebSocket feed. Every
 * update names the sequence number of the message before it; if that is not
 * the last one applied, something was missed, so the feed drops the
 * connection and starts again from a fresh snapshot.
 */
class MarketFeed {
  private ws: WebSocket | null = null;
  private listeners = new Set<() => void>();
  private frame = 0;
  private retry = 0;
  private retryTimer = 0;
  private closed = false;
  private view: MarketView;

  constructor(readonly symbol: string) {
    this.view = {
      status: "connecting",
      seq: 0,
      bids: new Map(),
      asks: new Map(),
      changed: new Map(),
      trades: [],
      resyncs: 0,
      version: 0,
    };
    this.loadHistory();
    this.connect();
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getSnapshot = () => this.view;

  close() {
    this.closed = true;
    clearTimeout(this.retryTimer);
    cancelAnimationFrame(this.frame);
    this.ws?.close();
  }

  private async loadHistory() {
    try {
      const history = await api.trades(this.symbol);
      if (this.closed) return;
      const known = new Set(this.view.trades.map((t) => t.seq));
      const merged = [...history.filter((t) => !known.has(t.seq)), ...this.view.trades].sort((a, b) => a.seq - b.seq);
      this.publish({ trades: merged.slice(-MAX_TRADES) });
    } catch {
      // History is a nicety; the live feed still works without it.
    }
  }

  private connect() {
    const ws = new WebSocket(`${WS_URL}/v1/ws/${encodeURIComponent(this.symbol)}?depth=200`);
    this.ws = ws;
    ws.onmessage = (ev) => this.onMessage(ev.data as string);
    ws.onclose = (ev) => {
      if (this.ws !== ws || this.closed) return;
      if (ev.code === CLOSE_TRY_AGAIN || this.view.status === "resyncing") {
        this.reconnect(0, "resyncing");
      } else {
        this.retry = Math.min(this.retry + 1, 5);
        this.reconnect(this.retry * 1000, "offline");
      }
    };
  }

  private reconnect(delay: number, status: FeedStatus) {
    this.publish({ status });
    clearTimeout(this.retryTimer);
    this.retryTimer = window.setTimeout(() => this.connect(), delay);
  }

  private resync() {
    this.publish({ status: "resyncing", resyncs: this.view.resyncs + 1 });
    this.ws?.close();
  }

  private onMessage(raw: string) {
    const msg = JSON.parse(raw) as Snapshot | Update;
    if (msg.type === "snapshot") {
      this.retry = 0;
      const trades = mergeTrades(this.view.trades, msg.trades ?? []);
      this.publish({
        status: "live",
        seq: msg.seq,
        bids: new Map(msg.bids),
        asks: new Map(msg.asks),
        changed: new Map(),
        trades,
      });
      return;
    }
    if (this.view.status !== "live") return;
    if (msg.prev !== this.view.seq) {
      this.resync();
      return;
    }
    const v = this.view;
    const now = performance.now();
    for (const d of msg.deltas ?? []) {
      const levels = d.side === "buy" ? v.bids : v.asks;
      if (d.qty === 0) levels.delete(d.price);
      else levels.set(d.price, d.qty);
      v.changed.set(levelKey(d.side, d.price), now);
    }
    if (msg.trades?.length) {
      v.trades.push(...msg.trades);
      if (v.trades.length > MAX_TRADES * 1.2) v.trades = v.trades.slice(-MAX_TRADES);
    }
    v.seq = msg.seq;
    this.schedule();
  }

  private publish(patch: Partial<MarketView>) {
    Object.assign(this.view, patch);
    this.schedule();
  }

  // Updates can arrive far faster than the screen refreshes; render at most
  // once per frame, with a new object so React sees the change.
  private schedule() {
    if (this.frame) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = 0;
      this.view = { ...this.view, version: this.view.version + 1 };
      for (const fn of this.listeners) fn();
    });
  }
}

export function levelKey(side: Side, price: number) {
  return `${side}:${price}`;
}

function mergeTrades(have: Trade[], incoming: Trade[]): Trade[] {
  if (!incoming.length) return have;
  const known = new Set(have.map((t) => t.seq));
  return [...have, ...incoming.filter((t) => !known.has(t.seq))].sort((a, b) => a.seq - b.seq).slice(-MAX_TRADES);
}

const idle: MarketView = {
  status: "connecting",
  seq: 0,
  bids: new Map(),
  asks: new Map(),
  changed: new Map(),
  trades: [],
  resyncs: 0,
  version: 0,
};
const noop = () => () => {};

export function useMarket(symbol: string): MarketView {
  const [feed, setFeed] = useState<MarketFeed | null>(null);
  useEffect(() => {
    const f = new MarketFeed(symbol);
    setFeed(f);
    return () => f.close();
  }, [symbol]);
  const live = feed?.symbol === symbol ? feed : null;
  return useSyncExternalStore(live?.subscribe ?? noop, live?.getSnapshot ?? (() => idle));
}
