import { API_URL } from "./config";
import type { OpenOrder, OrderResult, Side, Stats, SymbolInfo, Trade } from "./types";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(method: string, path: string, trader?: number, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (trader !== undefined) headers["X-Trader-ID"] = String(trader);
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let res: Response;
  try {
    res = await fetch(API_URL + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, "network", "Server unreachable");
  }
  const data = await res.json().catch(() => null);
  // Engine rejections (422, 403, 404 on cancel) still carry an order result.
  if (data && typeof data === "object" && "status" in data && "order_id" in data) return data as T;
  if (!res.ok) {
    const err = data as { error?: string; message?: string } | null;
    throw new ApiError(res.status, err?.error ?? "http_error", err?.message ?? `HTTP ${res.status}`);
  }
  return data as T;
}

export const api = {
  symbols: () => request<SymbolInfo[]>("GET", "/v1/symbols"),
  trades: (symbol: string, limit = 500) => request<Trade[]>("GET", `/v1/trades/${symbol}?limit=${limit}`),
  stats: () => request<Stats>("GET", "/v1/stats"),
  openOrders: (symbol: string, trader: number) => request<OpenOrder[]>("GET", `/v1/orders?symbol=${symbol}`, trader),
  place: (
    trader: number,
    order: {
      symbol: string;
      side: Side;
      type: "limit" | "market";
      tif?: string;
      post_only?: boolean;
      price?: number;
      qty: number;
      client_order_id?: string;
    },
  ) => request<OrderResult>("POST", "/v1/orders", trader, order),
  cancel: (trader: number, id: number) => request<OrderResult>("DELETE", `/v1/orders/${id}`, trader),
};
