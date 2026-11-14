export type Side = "buy" | "sell";

export interface SymbolInfo {
  symbol: string;
  price_scale: number;
  last: number;
  open: number;
  high: number;
  low: number;
  volume: number;
  trades: number;
  best_bid: number;
  best_ask: number;
}

export interface Trade {
  seq: number;
  time: number;
  price: number;
  qty: number;
  side: Side;
  maker_order_id: number;
  taker_order_id: number;
}

export interface Snapshot {
  type: "snapshot";
  symbol: string;
  seq: number;
  bids: [number, number][];
  asks: [number, number][];
  trades?: Trade[];
}

export interface Update {
  type: "update";
  symbol: string;
  seq: number;
  prev: number;
  deltas: { side: Side; price: number; qty: number }[] | null;
  trades: Trade[] | null;
}

export interface OpenOrder {
  order_id: number;
  side: Side;
  price: number;
  qty: number;
  post_only: boolean;
}

export interface OrderResult {
  order_id: number;
  symbol: string;
  status: "resting" | "filled" | "cancelled" | "rejected";
  reason?: string;
  filled: number;
  remaining: number;
}

export interface Latency {
  p50: number;
  p99: number;
  p999: number;
}

export interface SymbolStats {
  symbol: string;
  orders_per_sec: number;
  commands: number;
  fills: number;
  seq: number;
  queue_depth: number;
  latency_ns: Latency;
  wal: { durable: boolean; written: number; synced: number; lag: number; snapshot_index: number };
  feed: { subscribers: number; dropped: number; overruns: number; resyncs: number };
}

export interface Stats {
  at: number;
  window_ms: number;
  orders_per_sec: number;
  commands: number;
  fills: number;
  latency_ns: Latency;
  symbols: SymbolStats[];
  info?: Record<string, string>;
}
