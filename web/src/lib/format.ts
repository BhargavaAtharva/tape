const formatters = new Map<number, Intl.NumberFormat>();

function formatter(scale: number): Intl.NumberFormat {
  let f = formatters.get(scale);
  if (!f) {
    f = new Intl.NumberFormat("en-US", { minimumFractionDigits: scale, maximumFractionDigits: scale });
    formatters.set(scale, f);
  }
  return f;
}

export function formatPrice(ticks: number, scale: number): string {
  return formatter(scale).format(ticks / 10 ** scale);
}

/** parsePrice turns a display price into integer ticks, or null if it is not a valid price at this scale. */
export function parsePrice(input: string, scale: number): number | null {
  const s = input.trim().replace(/,/g, "");
  if (!/^\d+(\.\d*)?$/.test(s)) return null;
  const [, frac = ""] = s.split(".");
  if (frac.length > scale) return null;
  const ticks = Math.round(Number(s) * 10 ** scale);
  return ticks > 0 && Number.isSafeInteger(ticks) ? ticks : null;
}

const qtyFmt = new Intl.NumberFormat("en-US");
export const formatQty = (q: number) => qtyFmt.format(q);

export function formatChange(last: number, open: number): string {
  if (!open || !last) return "0.00%";
  const pct = ((last - open) / open) * 100;
  return `${pct >= 0 ? "+" : ""}${pct.toFixed(2)}%`;
}

export function formatTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

/** formatLatency renders nanoseconds with a unit that keeps three significant digits. */
export function formatLatency(ns: number): string {
  if (!ns) return "–";
  if (ns < 1e3) return `${ns} ns`;
  if (ns < 1e6) return `${(ns / 1e3).toPrecision(3)} µs`;
  if (ns < 1e9) return `${(ns / 1e6).toPrecision(3)} ms`;
  return `${(ns / 1e9).toPrecision(3)} s`;
}

export function formatRate(n: number): string {
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e4) return `${(n / 1e3).toFixed(1)}k`;
  return n.toFixed(0);
}
