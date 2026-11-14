import { useEffect, useMemo, useRef, useState } from "react";
import { formatPrice, formatQty } from "../lib/format";
import type { Trade } from "../lib/types";
import { Empty, Panel } from "./Panel";
import styles from "./PriceChart.module.css";

interface Candle {
  t: number;
  o: number;
  h: number;
  l: number;
  c: number;
  v: number;
}

const INTERVALS = [
  { label: "5s", ms: 5_000 },
  { label: "15s", ms: 15_000 },
  { label: "1m", ms: 60_000 },
];

const AXIS_W = 72;
const AXIS_H = 18;
const SLOT = 9;

function candles(trades: Trade[], interval: number): Candle[] {
  const out: Candle[] = [];
  for (const t of trades) {
    const bucket = Math.floor(t.time / interval) * interval;
    const last = out.at(-1);
    if (last && last.t === bucket) {
      last.h = Math.max(last.h, t.price);
      last.l = Math.min(last.l, t.price);
      last.c = t.price;
      last.v += t.qty;
    } else {
      // Each candle opens at the previous close so the series has no gaps.
      const open = last?.c ?? t.price;
      out.push({ t: bucket, o: open, h: Math.max(open, t.price), l: Math.min(open, t.price), c: t.price, v: t.qty });
    }
  }
  return out;
}

/** niceStep picks a grid spacing of 1, 2 or 5 times a power of ten. */
function niceStep(range: number, lines: number): number {
  const raw = range / lines;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const m = raw / pow;
  return (m < 1.5 ? 1 : m < 3.5 ? 2 : m < 7.5 ? 5 : 10) * pow;
}

function css(el: Element, name: string) {
  return getComputedStyle(el).getPropertyValue(name).trim();
}

function clock(ms: number) {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}:${String(d.getSeconds()).padStart(2, "0")}`;
}

interface Props {
  trades: Trade[];
  version: number;
  scale: number;
}

export function PriceChart({ trades, version, scale }: Props) {
  const [interval, setBucket] = useState(INTERVALS[0]!.ms);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ w: 0, h: 0 });
  const [hover, setHover] = useState<number | null>(null);
  const [scheme, setScheme] = useState(0);

  // The feed appends to trades in place, so version is what signals new data.
  const data = useMemo(() => candles(trades, interval), [trades, version, interval]);
  const visible = Math.max(1, Math.floor((size.w - AXIS_W) / SLOT));
  const shown = data.slice(-visible);
  const hovered = hover !== null ? shown[hover] : shown.at(-1);

  useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => {
      if (e) setSize({ w: Math.floor(e.contentRect.width), h: Math.floor(e.contentRect.height) });
    });
    ro.observe(el);
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const onScheme = () => setScheme((n) => n + 1);
    mq.addEventListener("change", onScheme);
    const mo = new MutationObserver(onScheme);
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => {
      ro.disconnect();
      mq.removeEventListener("change", onScheme);
      mo.disconnect();
    };
  }, []);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || size.w === 0 || size.h === 0) return;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = size.w * dpr;
    canvas.height = size.h * dpr;
    const ctx = canvas.getContext("2d")!;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, size.w, size.h);
    if (!shown.length) return;

    const colors = {
      line: css(canvas, "--line"),
      text: css(canvas, "--text-3"),
      label: css(canvas, "--text-2"),
      up: css(canvas, "--bid"),
      down: css(canvas, "--ask"),
      raised: css(canvas, "--raised"),
      mono: css(canvas, "--mono"),
    };
    const plotW = size.w - AXIS_W;
    const plotH = size.h - AXIS_H;
    const volH = plotH * 0.18;
    let lo = Math.min(...shown.map((c) => c.l));
    let hi = Math.max(...shown.map((c) => c.h));
    if (hi === lo) {
      hi += 10 ** scale * 0.01 || 1;
      lo -= 10 ** scale * 0.01 || 1;
    }
    const pad = (hi - lo) * 0.08;
    lo -= pad;
    hi += pad;
    const y = (p: number) => 6 + ((hi - p) / (hi - lo)) * (plotH - volH - 12);
    const x0 = plotW - shown.length * SLOT;

    ctx.font = `10px ${colors.mono}`;
    ctx.textBaseline = "middle";
    ctx.lineWidth = 1;

    const lastY = y(shown.at(-1)!.c);
    const step = niceStep(hi - lo, 5);
    ctx.strokeStyle = colors.line;
    ctx.fillStyle = colors.text;
    for (let p = Math.ceil(lo / step) * step; p <= hi; p += step) {
      const yy = Math.round(y(p)) + 0.5;
      ctx.beginPath();
      ctx.moveTo(0, yy);
      ctx.lineTo(plotW, yy);
      ctx.stroke();
      // The last-price tag is drawn over the axis; skip labels it would cover.
      if (Math.abs(yy - lastY) > 12) ctx.fillText(formatPrice(Math.round(p), scale), plotW + 6, yy);
    }

    const labelEvery = Math.max(1, Math.round(90 / SLOT));
    ctx.textAlign = "center";
    shown.forEach((c, i) => {
      const cx = x0 + i * SLOT + SLOT / 2;
      if ((data.length - shown.length + i) % labelEvery === 0) {
        ctx.fillStyle = colors.text;
        ctx.fillText(clock(c.t), cx, plotH + AXIS_H / 2);
      }
    });
    ctx.textAlign = "left";

    const maxV = Math.max(...shown.map((c) => c.v), 1);
    shown.forEach((c, i) => {
      const cx = Math.round(x0 + i * SLOT + SLOT / 2) + 0.5;
      const up = c.c >= c.o;
      const color = up ? colors.up : colors.down;
      ctx.globalAlpha = 0.28;
      ctx.fillStyle = color;
      const vh = (c.v / maxV) * volH;
      ctx.fillRect(cx - 3, plotH - vh, 6, vh);
      ctx.globalAlpha = 1;
      ctx.strokeStyle = color;
      ctx.beginPath();
      ctx.moveTo(cx, Math.round(y(c.h)));
      ctx.lineTo(cx, Math.round(y(c.l)));
      ctx.stroke();
      const top = Math.round(y(Math.max(c.o, c.c)));
      const h = Math.max(1, Math.round(y(Math.min(c.o, c.c))) - top);
      ctx.fillRect(cx - 3, top, 7, h);
    });

    const last = shown.at(-1)!;
    const ly = Math.round(y(last.c)) + 0.5;
    ctx.setLineDash([2, 3]);
    ctx.strokeStyle = colors.label;
    ctx.beginPath();
    ctx.moveTo(0, ly);
    ctx.lineTo(plotW, ly);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = last.c >= last.o ? colors.up : colors.down;
    ctx.fillRect(plotW + 1, ly - 8, AXIS_W - 1, 16);
    ctx.fillStyle = css(canvas, "--bg");
    ctx.fillText(formatPrice(last.c, scale), plotW + 6, ly);

    if (hover !== null && shown[hover]) {
      const cx = Math.round(x0 + hover * SLOT + SLOT / 2) + 0.5;
      ctx.strokeStyle = colors.label;
      ctx.globalAlpha = 0.5;
      ctx.beginPath();
      ctx.moveTo(cx, 0);
      ctx.lineTo(cx, plotH);
      ctx.stroke();
      ctx.globalAlpha = 1;
    }
    ctx.strokeStyle = colors.line;
    ctx.beginPath();
    ctx.moveTo(plotW + 0.5, 0);
    ctx.lineTo(plotW + 0.5, size.h);
    ctx.moveTo(0, plotH + 0.5);
    ctx.lineTo(plotW, plotH + 0.5);
    ctx.stroke();
  }, [shown, size, hover, scale, scheme, data.length]);

  const onMove = (e: React.PointerEvent) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const i = Math.floor((x - (size.w - AXIS_W - shown.length * SLOT)) / SLOT);
    setHover(i >= 0 && i < shown.length ? i : null);
  };

  const extra = (
    <div className={styles.intervals} role="group" aria-label="Candle interval">
      {INTERVALS.map((iv) => (
        <button
          key={iv.ms}
          type="button"
          className={iv.ms === interval ? styles.active : undefined}
          aria-pressed={iv.ms === interval}
          onClick={() => setBucket(iv.ms)}
        >
          {iv.label}
        </button>
      ))}
    </div>
  );

  return (
    <Panel title="Price" extra={extra} className={styles.panel}>
      {hovered && (
        <div className={`num ${styles.legend}`}>
          <span>{clock(hovered.t)}</span>
          <span>
            <i>O</i>
            {formatPrice(hovered.o, scale)}
          </span>
          <span>
            <i>H</i>
            {formatPrice(hovered.h, scale)}
          </span>
          <span>
            <i>L</i>
            {formatPrice(hovered.l, scale)}
          </span>
          <span className={hovered.c >= hovered.o ? "bid" : "ask"}>
            <i>C</i>
            {formatPrice(hovered.c, scale)}
          </span>
          <span>
            <i>V</i>
            {formatQty(hovered.v)}
          </span>
        </div>
      )}
      <div ref={wrapRef} className={styles.plot} onPointerMove={onMove} onPointerLeave={() => setHover(null)}>
        <canvas ref={canvasRef} style={{ width: size.w, height: size.h }} aria-label="Candlestick chart of recent trades" role="img" />
        {!data.length && (
          <div className={styles.overlay}>
            <Empty>No trades yet</Empty>
          </div>
        )}
      </div>
    </Panel>
  );
}
