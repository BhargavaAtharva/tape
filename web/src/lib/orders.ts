import { useEffect, useState } from "react";
import { api, ApiError } from "./api";
import { WS_URL } from "./config";
import type { OpenOrder, Report } from "./types";

export interface MyOrders {
  orders: OpenOrder[] | null;
  error: string;
  /** Every order id this trader has had on this symbol, for marking trades. */
  own: Set<number>;
}

/**
 * useMyOrders keeps the trader's open orders for one symbol from the private
 * execution-report stream. The list is loaded once over REST each time the
 * stream (re)connects, then maintained from acks, fills, amends and cancels,
 * so nothing is polled. Like the market feed it disconnects while the page
 * is hidden.
 */
export function useMyOrders(symbol: string, trader: number): MyOrders {
  const [state, setState] = useState<MyOrders>({ orders: null, error: "", own: new Set() });

  useEffect(() => {
    if (!symbol) return;
    let ws: WebSocket | null = null;
    let closed = false;
    let retry = 0;
    let timer = 0;
    const open = new Map<number, OpenOrder>();
    const own = new Set<number>();
    const publish = (error = "") =>
      setState({ orders: [...open.values()].sort((a, b) => a.order_id - b.order_id), error, own: new Set(own) });

    const load = async () => {
      try {
        const list = await api.openOrders(symbol, trader);
        if (closed) return;
        open.clear();
        for (const o of list) {
          open.set(o.order_id, o);
          own.add(o.order_id);
        }
        publish();
      } catch (err) {
        if (!closed) publish(err instanceof ApiError ? err.message : "Could not load orders");
      }
    };

    const apply = (r: Report) => {
      if (r.symbol !== symbol || !r.order_id) return;
      own.add(r.order_id);
      const o = open.get(r.order_id);
      switch (r.kind) {
        case "accepted":
          // Removed again by the fill or cancel that follows if it never rests.
          open.set(r.order_id, { order_id: r.order_id, side: r.side!, price: r.price ?? 0, qty: r.qty, filled: 0, post_only: false });
          break;
        case "fill":
          if (!o) break;
          if (r.remaining === 0) open.delete(r.order_id);
          else open.set(r.order_id, { ...o, qty: r.remaining, filled: o.filled + r.qty });
          break;
        case "amended":
          if (o) open.set(r.order_id, { ...o, price: r.price ?? o.price, qty: r.qty });
          break;
        case "cancelled":
          open.delete(r.order_id);
          break;
      }
    };

    // Reports arrive in bursts (an ack, fills, a cancel); render once per frame.
    let frame = 0;
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(() => ((frame = 0), publish()));
    };

    const connect = () => {
      const sock = new WebSocket(`${WS_URL}/v1/ws/private?trader=${trader}`);
      ws = sock;
      sock.onopen = () => {
        retry = 0;
        load();
      };
      sock.onmessage = (e) => {
        apply(JSON.parse(e.data as string) as Report);
        schedule();
      };
      sock.onclose = () => {
        if (ws !== sock || closed) return;
        retry = Math.min(retry + 1, 5);
        timer = window.setTimeout(connect, retry * 1000);
      };
    };

    const onVisibility = () => {
      if (document.hidden) {
        clearTimeout(timer);
        const sock = ws;
        ws = null;
        sock?.close();
      } else if (!ws) {
        connect();
      }
    };

    setState({ orders: null, error: "", own: new Set() });
    document.addEventListener("visibilitychange", onVisibility);
    if (!document.hidden) connect();
    return () => {
      closed = true;
      clearTimeout(timer);
      cancelAnimationFrame(frame);
      document.removeEventListener("visibilitychange", onVisibility);
      ws?.close();
    };
  }, [symbol, trader]);

  return state;
}
