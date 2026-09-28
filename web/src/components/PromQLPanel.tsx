// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/components/PromQLPanel.tsx — Copyright the ongrid authors, AGPL-3.0
// (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent:
//   - colors come from this console's theme tokens (light + dark) and a
//     validated categorical palette instead of fixed zinc/hex values;
//   - targets' legendFormat names the series ({{label}} templating);
//   - stat / list bodies honor Grafana value mappings (e.g. 1 → UP), so a
//     state never reads by color alone;
//   - the "open in Grafana" deep-link affordances were dropped;
//   - a changed time window (custom range) refetches, not only a new tick;
//   - strings are Chinese-only (no i18n layer in this console).

import { useEffect, useMemo, useRef, useState } from 'react';
import { Loader2, AlertTriangle } from 'lucide-react';
import {
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { ApiError } from '../api';
import { queryRange, type PromMatrixSeries } from '../lib/prom';
import type { GrafanaPanel } from '../lib/grafana';
import { stepForRange, substitute, type VarContext } from '../lib/grafanaVars';

// PromQLPanel renders ONE Grafana panel as a native chart by walking
// `panel.targets[]`, running each through /api/v1/prometheus/query_range,
// and pivoting the resulting Prom matrices into a recharts-friendly
// row form.
//
// Panel-type coverage (matches Monitor.tsx fallback policy):
//   timeseries / graph    → LineChart
//   stat                  → big-number with threshold-driven color
//   gauge                 → bar fill 0..100% (or 0..max from thresholds)
//   bargauge / table      → compact (label → value) rows, latest only
//   <anything else>       → "暂未支持" card
//
// We DON'T fall back to iframe for unsupported types — the whole point
// of this renderer is to break the iframe dependency.

// Categorical series slots, defined per theme in index.css (--series-1..6).
const SERIES_COLORS = [1, 2, 3, 4, 5, 6].map((slot) => `var(--series-${slot})`);

// Threshold default for stat / gauge when the dashboard doesn't carry
// fieldConfig.defaults.thresholds.
const DEFAULT_THRESHOLD_COLOR = 'var(--fg)';

const RENDERABLE_TIMESERIES = new Set(['timeseries', 'graph']);

export type PromQLPanelProps = {
  panel: GrafanaPanel;
  range: string;
  // tick is bumped by Monitor on user-driven refresh AND by the auto-
  // refresh interval; the panel re-runs all queries when it changes.
  tick: number;
  fromMs: number;
  toMs: number;
};

// PanelSeries is one Prom series plus its legend text resolved from the
// target's legendFormat.
type PanelSeries = PromMatrixSeries & { legend: string };

type PanelStatus =
  | { kind: 'loading' }
  | { kind: 'empty' }
  | { kind: 'error'; msg: string }
  | { kind: 'data'; matrix: PanelSeries[] };

export function PromQLPanel(props: PromQLPanelProps) {
  const { panel, range, tick, fromMs, toMs } = props;

  const [status, setStatus] = useState<PanelStatus>({ kind: 'loading' });
  // Ref kept so a stale fetch arriving after a refresh doesn't overwrite
  // newer state — we discard responses whose token doesn't match.
  const fetchToken = useRef(0);

  const targets = useMemo(
    () => (Array.isArray(panel.targets) ? panel.targets.filter((t) => (t.expr ?? '').trim() !== '') : []),
    [panel],
  );
  // Stable string key over every target's expr — used as a useEffect
  // dep so a parent rebuilding `panel` with a new PromQL triggers a
  // refetch. panel.id alone misses this.
  const exprKey = useMemo(() => targets.map((t) => t.expr ?? '').join(' '), [targets]);

  const isSupported = supportedType(panel.type);

  useEffect(() => {
    if (!isSupported || targets.length === 0) {
      // For unsupported types we skip the fetch entirely. For zero-target
      // panels (rare, mostly text/canvas) we render an empty placeholder.
      setStatus({ kind: 'empty' });
      return;
    }
    const myToken = ++fetchToken.current;
    setStatus({ kind: 'loading' });

    const ctx: VarContext = {
      range,
      query: new URLSearchParams(window.location.search),
    };
    const start = new Date(fromMs).toISOString();
    const end = new Date(toMs).toISOString();
    const step = stepForRange(range);

    Promise.all(
      targets.map(async (t) => {
        const expr = substitute(t.expr ?? '', ctx);
        const resp = await queryRange({ expr, start, end, step });
        return (resp.result ?? []).map((s, i) => ({ ...s, legend: legendFor(t.legendFormat, s.metric, i) }));
      }),
    )
      .then((perTarget) => {
        if (fetchToken.current !== myToken) return;
        const merged = perTarget.flat();
        if (merged.length === 0) {
          setStatus({ kind: 'empty' });
          return;
        }
        setStatus({ kind: 'data', matrix: merged });
      })
      .catch((err) => {
        if (fetchToken.current !== myToken) return;
        const msg = err instanceof ApiError ? err.message : (err as Error).message || 'PromQL 查询失败';
        setStatus({ kind: 'error', msg });
      });
    // tick + range + panel.id are the obvious refetch triggers; exprKey
    // covers the case where the expr is rewritten in place.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tick, range, panel.id, isSupported, exprKey, fromMs, toMs]);

  return (
    <PanelCard panel={panel}>
      <PanelBody {...props} status={status} />
    </PanelCard>
  );
}

function supportedType(type: string): boolean {
  if (RENDERABLE_TIMESERIES.has(type)) return true;
  if (type === 'stat' || type === 'gauge') return true;
  if (type === 'bargauge' || type === 'table') return true;
  return false;
}

// PanelCard is the chrome — title and description — common to every
// panel type. Body is what changes.
function PanelCard({ panel, children }: { panel: GrafanaPanel; children: React.ReactNode }) {
  return (
    <section aria-label={panel.title} className="flex h-full w-full flex-col overflow-hidden rounded-lg border border-line bg-surface">
      <header className="flex items-center justify-between gap-2 border-b border-line-soft px-3 py-2">
        <div className="min-w-0">
          <h3 title={panel.title} className="truncate text-xs font-medium text-fg">
            {panel.title || `Panel #${panel.id}`}
          </h3>
          {panel.description ? (
            <p title={panel.description} className="truncate text-[10px] text-fg-faint">
              {panel.description}
            </p>
          ) : null}
          {/* Show the effective PromQL on hover — operators can verify
              what the panel actually queries. */}
          {panel.targets?.[0]?.expr ? (
            <p title={panel.targets[0].expr} className="truncate font-mono text-[10px] text-fg-faint">
              {panel.targets[0].expr}
            </p>
          ) : null}
        </div>
      </header>
      <div className="flex-1 overflow-hidden p-2">{children}</div>
    </section>
  );
}

function PanelBody(props: PromQLPanelProps & { status: PanelStatus }) {
  const { panel, status } = props;

  if (!supportedType(panel.type)) {
    return <UnsupportedPanel panel={panel} />;
  }

  if (status.kind === 'loading') {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-xs text-fg-faint">
        <Loader2 size={12} className="animate-spin" />
        <span>加载中…</span>
      </div>
    );
  }
  if (status.kind === 'error') {
    return (
      <div role="alert" className="flex h-full flex-col items-center justify-center gap-1 px-2 text-center text-xs">
        <AlertTriangle size={14} className="text-danger" />
        <p className="font-medium text-danger">查询失败</p>
        <p title={status.msg} className="line-clamp-2 break-all text-[10px] text-fg-faint">
          {status.msg}
        </p>
      </div>
    );
  }
  if (status.kind === 'empty') {
    const expr = (panel.targets ?? []).map((t) => t.expr ?? '').filter(Boolean).join(' │ ');
    return (
      <div className="flex h-full flex-col items-center justify-center gap-1 px-2 text-center">
        <p className="text-xs text-fg-muted">无数据</p>
        {expr ? (
          <code className="line-clamp-2 break-all rounded bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] text-fg-faint">
            {expr}
          </code>
        ) : null}
      </div>
    );
  }

  // status.kind === 'data'
  if (RENDERABLE_TIMESERIES.has(panel.type)) {
    return <TimeseriesBody panel={panel} matrix={status.matrix} fromMs={props.fromMs} toMs={props.toMs} range={props.range} />;
  }
  if (panel.type === 'stat') {
    return <StatBody panel={panel} matrix={status.matrix} />;
  }
  if (panel.type === 'gauge') {
    return <GaugeBody panel={panel} matrix={status.matrix} />;
  }
  // bargauge / table
  return <ListBody panel={panel} matrix={status.matrix} />;
}

// ---------- timeseries ----------

type ChartRow = { ts: number; tsLabel: string } & Record<string, number | null | string>;

function TimeseriesBody({ panel, matrix, fromMs, toMs, range }: { panel: GrafanaPanel; matrix: PanelSeries[]; fromMs: number; toMs: number; range: string }) {
  const { rows, series } = useMemo(() => matrixToRows(matrix, stepMsForRange(range)), [matrix, range]);
  const unit = panel.fieldConfig?.defaults?.unit ?? '';
  const formatY = useMemo(() => buildUnitFormatter(unit), [unit]);
  // Hide the legend on dense panels — at >6 series Grafana itself
  // collapses to a tooltip-only display, and recharts' legend wraps
  // ugly when it gets long.
  const showLegend = series.length > 0 && series.length <= 6;

  return (
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={rows} margin={{ top: 4, right: 8, bottom: 0, left: -8 }}>
        <CartesianGrid stroke="var(--line-soft)" vertical={false} />
        <XAxis
          // Honest time axis: domain = the user-selected range, NOT
          // the data extent, so ingest gaps at the edges stay visible.
          dataKey="ts"
          type="number"
          scale="time"
          domain={[fromMs, toMs]}
          tickFormatter={(v: number) => formatTsLabel(v)}
          stroke="var(--line)"
          tick={{ fontSize: 10, fill: 'var(--fg-faint)' }}
          interval="preserveStartEnd"
          minTickGap={36}
        />
        <YAxis
          stroke="var(--line)"
          tick={{ fontSize: 10, fill: 'var(--fg-faint)' }}
          width={56}
          tickFormatter={(v) => formatY(Number(v))}
        />
        <Tooltip
          contentStyle={{
            background: 'var(--surface)',
            border: '1px solid var(--line)',
            borderRadius: 8,
            fontSize: 11,
            color: 'var(--fg)',
            padding: '6px 8px',
          }}
          labelStyle={{ color: 'var(--fg-muted)', marginBottom: 4 }}
          itemStyle={{ padding: '0', lineHeight: '14px', color: 'var(--fg)' }}
          labelFormatter={(v) => formatTsLabel(Number(v))}
          formatter={(value, name) => {
            const desc = series.find((s) => s.key === String(name));
            const v = typeof value === 'number' ? formatY(value) : String(value);
            return [v, desc?.label ?? String(name)];
          }}
        />
        {showLegend ? (
          <Legend
            wrapperStyle={{ fontSize: 10, color: 'var(--fg-muted)', paddingTop: 4 }}
            iconType="plainline"
            formatter={(value) => {
              const desc = series.find((s) => s.key === String(value));
              return <span style={{ color: 'var(--fg-muted)' }}>{desc?.label ?? String(value)}</span>;
            }}
          />
        ) : null}
        {series.map((s) => (
          <Line
            key={s.key}
            // Linear matches Grafana's default — keep the chart shape
            // identical to what operators see in Grafana itself.
            type="linear"
            dataKey={s.key}
            stroke={s.color}
            strokeWidth={2}
            dot={false}
            connectNulls={false}
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}

// stepMsForRange — parsed millisecond version of grafanaVars.stepForRange.
// We need the numeric value for gap-detection, not the Go-duration string.
function stepMsForRange(range: string): number {
  const m = /^(\d+)([smhdw])$/.exec(range.trim());
  if (!m) return 60_000;
  const n = parseInt(m[1], 10);
  const mult: Record<string, number> = { s: 1000, m: 60_000, h: 3600_000, d: 86_400_000, w: 604_800_000 };
  const ms = n * (mult[m[2]] ?? 3_600_000);
  return Math.max(15_000, Math.floor(ms / 360));
}

// matrixToRows pivots the series into the long-form recharts expects:
// one row per timestamp with a column per series.
function matrixToRows(matrix: PanelSeries[], stepMs: number = 60_000): {
  rows: ChartRow[];
  series: { key: string; label: string; color: string }[];
} {
  if (matrix.length === 0) return { rows: [], series: [] };

  // Build series descriptors. Key = stable per-series id; label = the
  // legend text resolved from the target's legendFormat.
  const series = matrix.map((m, i) => ({
    key: `s${i}`,
    label: m.legend,
    color: SERIES_COLORS[i % SERIES_COLORS.length],
  }));

  // Prom returns ts in unix SECONDS; we store ts in MILLISECONDS on
  // rows so it matches Monitor.tsx's fromMs/toMs domain (Date.now()).
  const tsSet = new Set<number>();
  for (const m of matrix) {
    for (const [tsSec] of m.values ?? []) tsSet.add(tsSec * 1000);
  }
  let tsList = Array.from(tsSet).sort((a, b) => a - b);

  // Gap injection: when two adjacent observed timestamps are more than
  // gapThresholdMs apart, insert a single phantom ts between them. Every
  // series's value at that phantom ts is null, and recharts' Line with
  // connectNulls={false} breaks the curve there — same visual semantics
  // as Grafana's missing-data break.
  //
  // Threshold: 2.5 × step. Looser than Grafana's 1.5× default to avoid
  // dotting healthy panels with breaks when one scrape arrives a beat
  // late; tight enough that an actual minutes-long outage shows.
  if (stepMs > 0 && tsList.length >= 2) {
    const gapThreshold = stepMs * 2.5;
    const withGaps: number[] = [tsList[0]];
    for (let i = 1; i < tsList.length; i++) {
      const prev = tsList[i - 1];
      const cur = tsList[i];
      if (cur - prev > gapThreshold) {
        // Phantom in the middle — keeps the broken segment visible
        // without flooding the row count.
        withGaps.push(prev + Math.floor((cur - prev) / 2));
      }
      withGaps.push(cur);
    }
    tsList = withGaps;
  }

  const rows: ChartRow[] = tsList.map((tsMs) => {
    const row: ChartRow = { ts: tsMs, tsLabel: formatTsLabel(tsMs) };
    for (let i = 0; i < matrix.length; i++) {
      row[series[i].key] = null;
    }
    return row;
  });
  const rowByMs = new Map<number, ChartRow>();
  for (const r of rows) rowByMs.set(r.ts, r);

  for (let i = 0; i < matrix.length; i++) {
    for (const [tsSec, raw] of matrix[i].values ?? []) {
      const v = parseFloat(raw);
      if (!Number.isFinite(v)) continue; // NaN / Inf → keep as null gap
      const row = rowByMs.get(tsSec * 1000);
      if (row) row[series[i].key] = v;
    }
  }
  return { rows, series };
}

function formatTsLabel(tsMs: number): string {
  const d = new Date(tsMs);
  const hh = d.getHours().toString().padStart(2, '0');
  const mm = d.getMinutes().toString().padStart(2, '0');
  return `${hh}:${mm}`;
}

// ---------- stat ----------

function StatBody({ panel, matrix }: { panel: GrafanaPanel; matrix: PanelSeries[] }) {
  const latest = latestValue(matrix);
  const unit = panel.fieldConfig?.defaults?.unit ?? '';
  const formatV = useMemo(() => buildUnitFormatter(unit), [unit]);
  const mapped = mapValue(panel, latest);
  const color = mapped?.color ?? pickThresholdColor(panel.fieldConfig?.defaults?.thresholds, latest);

  if (latest == null) {
    return <div className="flex h-full items-center justify-center text-xs text-fg-faint">无数据</div>;
  }
  return (
    <div className="flex h-full flex-col items-center justify-center">
      <div className="text-2xl font-semibold" style={{ color }}>
        {mapped?.text ?? formatV(latest)}
      </div>
    </div>
  );
}

// ---------- gauge ----------

function GaugeBody({ panel, matrix }: { panel: GrafanaPanel; matrix: PanelSeries[] }) {
  const latest = latestValue(matrix);
  const def = panel.fieldConfig?.defaults ?? {};
  const unit = def.unit ?? '';
  const formatV = useMemo(() => buildUnitFormatter(unit), [unit]);

  // Range: prefer fieldConfig.defaults.{min,max}; fall back to 0..100 for
  // percent unit; else 0..(max threshold) when thresholds are present;
  // else 0..max(value, 1) so the bar shows something.
  const min = typeof def.min === 'number' ? def.min : 0;
  const max = (() => {
    if (typeof def.max === 'number') return def.max;
    if (unit === 'percent' || unit === 'percentunit') return unit === 'percentunit' ? 1 : 100;
    const steps = def.thresholds?.steps ?? [];
    const last = steps[steps.length - 1]?.value;
    if (typeof last === 'number' && last > 0) return last;
    return Math.max(latest ?? 1, 1);
  })();

  const color = pickThresholdColor(def.thresholds, latest);
  const pct = latest == null ? 0 : Math.max(0, Math.min(1, (latest - min) / (max - min)));

  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 px-3">
      <div className="text-xl font-semibold" style={{ color }}>
        {latest == null ? '—' : formatV(latest)}
      </div>
      <div className="h-2 w-full overflow-hidden rounded-full bg-surface-2">
        <div className="h-full" style={{ width: `${pct * 100}%`, background: color }} />
      </div>
      <div className="tabular flex w-full justify-between text-[10px] text-fg-faint">
        <span>{formatV(min)}</span>
        <span>{formatV(max)}</span>
      </div>
    </div>
  );
}

// ---------- list (bargauge / table) ----------

function ListBody({ panel, matrix }: { panel: GrafanaPanel; matrix: PanelSeries[] }) {
  const unit = panel.fieldConfig?.defaults?.unit ?? '';
  const formatV = useMemo(() => buildUnitFormatter(unit), [unit]);
  // One row per series, latest value.
  const rows = matrix
    .map((m) => {
      const last = lastSample(m);
      if (last == null) return null;
      return { label: m.legend, value: last };
    })
    .filter((r): r is { label: string; value: number } => r !== null)
    .sort((a, b) => b.value - a.value)
    .slice(0, 12);
  if (rows.length === 0) {
    return <div className="flex h-full items-center justify-center text-xs text-fg-faint">无数据</div>;
  }
  return (
    <div className="h-full overflow-auto">
      <table className="w-full text-xs">
        <tbody>
          {rows.map((r, i) => {
            const mapped = mapValue(panel, r.value);
            return (
              <tr key={i} className="border-b border-line-soft last:border-0">
                <td className="truncate py-1 pr-2 text-fg-muted">{r.label || '—'}</td>
                <td className="tabular py-1 text-right font-medium" style={{ color: mapped?.color ?? 'var(--fg)' }}>
                  {mapped?.text ?? formatV(r.value)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ---------- unsupported ----------

function UnsupportedPanel({ panel }: { panel: GrafanaPanel }) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 px-3 text-center">
      <p className="text-xs text-fg-muted">
        暂未支持的面板类型 <code className="font-mono text-fg-faint">{panel.type}</code>
      </p>
    </div>
  );
}

// ---------- helpers ----------

// legendFor renders a target's legendFormat ("{{job}}") against one
// series' labels, falling back to every non-name label like Grafana does.
function legendFor(format: string | undefined, labels: Record<string, string> = {}, index: number): string {
  if (format && format.trim() !== '') {
    return format.replace(/\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g, (_, name: string) => labels[name] ?? '');
  }
  const entries = Object.entries(labels).filter(([k]) => k !== '__name__');
  return entries.length === 0 ? labels.__name__ || `series ${index}` : entries.map(([k, v]) => `${k}=${v}`).join(' ');
}

type ValueMapping = { type?: string; options?: Record<string, { text?: string; color?: string }> };

// mapValue applies Grafana "value" mappings from fieldConfig.defaults.
function mapValue(panel: GrafanaPanel, value: number | null): { text: string; color?: string } | null {
  if (value == null) return null;
  const mappings = (panel.fieldConfig?.defaults?.mappings ?? []) as ValueMapping[];
  for (const mapping of mappings) {
    if (mapping.type !== 'value') continue;
    const hit = mapping.options?.[String(value)];
    if (hit?.text) return { text: hit.text, color: hit.color ? mapGrafanaColor(hit.color) : undefined };
  }
  return null;
}

function lastSample(s: PromMatrixSeries): number | null {
  const values = s.values ?? [];
  for (let i = values.length - 1; i >= 0; i--) {
    const v = parseFloat(values[i][1]);
    if (Number.isFinite(v)) return v;
  }
  return null;
}

function latestValue(matrix: PromMatrixSeries[]): number | null {
  // Use the most-recent timestamped sample across all series. The
  // series with the largest last-ts wins; ties resolved by first-seen.
  let best: { ts: number; v: number } | null = null;
  for (const s of matrix) {
    const values = s.values ?? [];
    for (let i = values.length - 1; i >= 0; i--) {
      const v = parseFloat(values[i][1]);
      const ts = values[i][0];
      if (!Number.isFinite(v)) continue;
      if (!best || ts > best.ts) best = { ts, v };
      break;
    }
  }
  return best ? best.v : null;
}

// pickThresholdColor walks Grafana's thresholds.steps[] and returns the
// color of the highest step whose value is <= `value`. value=null → the
// "from -∞" base step (steps[0].value typically null in Grafana JSON).
function pickThresholdColor(
  thresholds: { mode?: string; steps?: Array<{ color: string; value: number | null }> } | undefined,
  value: number | null,
): string {
  const steps = thresholds?.steps ?? [];
  if (steps.length === 0) return DEFAULT_THRESHOLD_COLOR;
  if (value == null) return mapGrafanaColor(steps[0].color);
  let pick = steps[0];
  for (const step of steps) {
    if (step.value == null) continue;
    if (value >= step.value) pick = step;
  }
  return mapGrafanaColor(pick.color);
}

// Grafana color names → CSS. The status names map to this console's status
// tokens so they follow the light/dark theme; hex passes through.
function mapGrafanaColor(c: string): string {
  if (!c) return DEFAULT_THRESHOLD_COLOR;
  if (c.startsWith('#') || c.startsWith('rgb') || c.startsWith('var(')) return c;
  const m: Record<string, string> = {
    green: 'var(--ok)',
    red: 'var(--danger)',
    yellow: 'var(--warn)',
    orange: 'var(--warn)',
    blue: 'var(--info)',
    purple: 'var(--accent)',
    transparent: 'transparent',
  };
  return m[c] ?? DEFAULT_THRESHOLD_COLOR;
}

// buildUnitFormatter maps Grafana's unit strings to a value→string fn
// for tooltip / axis / stat display. Covers every unit our dashboards
// ship plus the most-common Grafana defaults. Anything else falls
// through to a plain number formatter — better than blanks.
function buildUnitFormatter(unit: string): (v: number) => string {
  const fmt = (v: number, suffix: string, decimals = 2) => {
    if (!Number.isFinite(v)) return '—';
    return `${v.toFixed(decimals).replace(/\.?0+$/, '')}${suffix}`;
  };
  switch (unit) {
    case 'percent':
      return (v) => fmt(v, '%', 1);
    case 'percentunit':
      return (v) => fmt(v * 100, '%', 1);
    case 'bytes':
    case 'decbytes':
      return (v) => formatBytes(v, 1024);
    case 'bytes_si':
      return (v) => formatBytes(v, 1000);
    case 'Bps':
    case 'binBps':
      return (v) => `${formatBytes(v, 1024)}/s`;
    case 'bps':
      return (v) => formatBitsPerSec(v);
    case 's':
      return (v) => formatSeconds(v);
    case 'ms':
      return (v) => fmt(v, ' ms', 1);
    case 'short':
    case 'none':
    case '':
      return (v) => fmt(v, '', 2);
    default:
      // Unknown unit — treat as opaque tag for transparency, but only
      // when it's <= 4 chars (anything longer is probably noise from
      // a panel type we don't fully understand).
      if (unit.length > 0 && unit.length <= 4) return (v) => fmt(v, ' ' + unit, 2);
      return (v) => fmt(v, '', 2);
  }
}

function formatBytes(v: number, base: number): string {
  if (!Number.isFinite(v)) return '—';
  const units = base === 1000 ? ['B', 'kB', 'MB', 'GB', 'TB', 'PB'] : ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let n = v;
  while (Math.abs(n) >= base && i < units.length - 1) {
    n /= base;
    i++;
  }
  return `${n.toFixed(2).replace(/\.?0+$/, '')} ${units[i]}`;
}

function formatBitsPerSec(v: number): string {
  if (!Number.isFinite(v)) return '—';
  const units = ['bps', 'Kbps', 'Mbps', 'Gbps', 'Tbps'];
  let i = 0;
  let n = v;
  while (Math.abs(n) >= 1000 && i < units.length - 1) {
    n /= 1000;
    i++;
  }
  return `${n.toFixed(2).replace(/\.?0+$/, '')} ${units[i]}`;
}

function formatSeconds(v: number): string {
  if (!Number.isFinite(v)) return '—';
  if (Math.abs(v) < 1e-3) return `${(v * 1e6).toFixed(0)} µs`;
  if (Math.abs(v) < 1) return `${(v * 1000).toFixed(0)} ms`;
  if (Math.abs(v) < 60) return `${v.toFixed(2).replace(/\.?0+$/, '')} s`;
  if (Math.abs(v) < 3600) return `${(v / 60).toFixed(1)} min`;
  return `${(v / 3600).toFixed(1)} h`;
}
