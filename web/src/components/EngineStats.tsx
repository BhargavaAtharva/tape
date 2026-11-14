import { formatLatency, formatRate } from "../lib/format";
import type { Stats } from "../lib/types";
import { Empty, Panel } from "./Panel";
import styles from "./EngineStats.module.css";

interface Props {
  stats: Stats | null;
  history: number[];
  symbol: string;
  error: boolean;
}

function Spark({ values }: { values: number[] }) {
  if (values.length < 2) return <svg className={styles.spark} aria-hidden="true" />;
  const max = Math.max(...values, 1);
  const pts = values.map((v, i) => `${(i / (values.length - 1)) * 100},${28 - (v / max) * 26}`).join(" ");
  return (
    <svg className={styles.spark} viewBox="0 0 100 30" preserveAspectRatio="none" aria-hidden="true">
      <polyline points={pts} fill="none" stroke="currentColor" strokeWidth="1" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

function Cell({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className={styles.cell}>
      <span className="label">{label}</span>
      <span className={`num ${styles.value}`}>{value}</span>
      {sub && <span className={`num ${styles.sub}`}>{sub}</span>}
    </div>
  );
}

export function EngineStats({ stats, history, symbol, error }: Props) {
  if (!stats) {
    return (
      <Panel title="Engine" className={styles.panel}>
        <Empty>{error ? "Stats unavailable" : "Loading"}</Empty>
      </Panel>
    );
  }
  const s = stats.symbols.find((x) => x.symbol === symbol);
  const sync = stats.info?.wal_sync ?? "unknown";
  const walValue = s?.wal.durable ? `${sync}` : "memory";
  const walSub = s?.wal.durable ? `lag ${s.wal.lag} · snap #${s.wal.snapshot_index}` : "not durable";
  return (
    <Panel
      title="Engine"
      extra={
        error ? <span className="ask">stale</span> : <span className="num">{stats.window_ms ? `${stats.window_ms} ms window` : ""}</span>
      }
      className={styles.panel}
    >
      <div className={styles.grid}>
        <div className={`${styles.cell} ${styles.wide}`}>
          <span className="label">Orders/s, all symbols</span>
          <span className={`num ${styles.value} ${styles.big}`}>{formatRate(stats.orders_per_sec)}</span>
          <Spark values={history} />
        </div>
        <Cell label="Latency p50" value={formatLatency(stats.latency_ns.p50)} />
        <Cell label="p99" value={formatLatency(stats.latency_ns.p99)} />
        <Cell label="p99.9" value={formatLatency(stats.latency_ns.p999)} />
        <Cell label={`${symbol} seq`} value={s ? String(s.seq) : "–"} sub={s ? `${formatRate(s.orders_per_sec)}/s` : undefined} />
        <Cell label="Fills" value={formatRate(stats.fills)} sub={`${formatRate(stats.commands)} cmds`} />
        <Cell label="WAL" value={walValue} sub={walSub} />
        <Cell
          label="Feed"
          value={s ? `${s.feed.subscribers} subs` : "–"}
          sub={s ? `${s.feed.dropped} dropped · ${s.feed.resyncs} resyncs` : undefined}
        />
      </div>
    </Panel>
  );
}
