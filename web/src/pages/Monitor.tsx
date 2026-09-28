// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/pages/Monitor.tsx — Copyright the ongrid authors, AGPL-3.0
// (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent:
//   - panels come from this deployment's four provisioned dashboards
//     (internal/grafana) picked with board tabs, instead of ongrid's
//     hardcoded fleet panels; logs panels are left to Grafana;
//   - the fleet role/device filters, user-managed panels, process top-N
//     and Grafana deep-links were not copied (single host, no edge fleet);
//   - a custom window sizes the query step and $__rate_interval from its
//     length and does not auto-refresh;
//   - header and toolbar use this console's PageHeader / Tabs / SelectInput,
//     strings are Chinese-only.

import { RefreshCw } from 'lucide-react';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router';

import { ApiError } from '../api';
import { fetchDashboard, listDashboards, type GrafanaDashboardSummary, type GrafanaPanel } from '../lib/grafana';
import { PageBody, PageHeader } from '../components/layout/PageHeader';
import { PanelGrid } from '../components/monitor/PanelGrid';
import { TimeRangePicker } from '../components/TimeRangePicker';
import { Button, Notice, SelectInput, Skeleton, Tabs } from '../components/ui';
import { flattenPanels } from '../lib/grafanaVars';
import { absoluteWindow } from '../lib/telemetryContext';
import { usePoll } from '../lib/usePoll';

// Monitor renders the provisioned dashboards natively — no iframe. PromQL
// runs against /api/v1/prometheus/query_range and recharts draws the
// lines, so the page works whether Grafana is up or not.

const RANGE_PRESETS: { value: string; label: string }[] = [
  { value: '15m', label: '15 分钟' },
  { value: '1h', label: '1 小时' },
  { value: '3h', label: '3 小时' },
  { value: '6h', label: '6 小时' },
  { value: '24h', label: '1 天' },
  { value: '3d', label: '3 天' },
  { value: '7d', label: '7 天' },
];
const DEFAULT_RANGE = '1h';

// Auto-refresh tick cadence (seconds). Bumps a tick state which threads
// down into PromQLPanel as a prop — each panel re-fetches when tick
// changes.
const REFRESH_PRESETS: { value: number; label: string }[] = [
  { value: 0, label: '关' },
  { value: 30, label: '30 秒' },
  { value: 60, label: '1 分钟' },
  { value: 300, label: '5 分钟' },
];
const DEFAULT_REFRESH = 60;

function rangeToMs(range: string): number {
  const m = /^(\d+)([smhdw])$/.exec(range.trim());
  if (!m) return 3600_000;
  const n = parseInt(m[1], 10);
  const mult: Record<string, number> = {
    s: 1000,
    m: 60_000,
    h: 3600_000,
    d: 86400_000,
    w: 604800_000,
  };
  return n * (mult[m[2]] ?? 3600_000);
}

// customRange picks the preset whose duration best matches an absolute
// window, so $__rate_interval and the query step scale with it.
function customRange(start: string, end: string): string {
  const span = Date.parse(end) - Date.parse(start);
  const fit = RANGE_PRESETS.find((preset) => rangeToMs(preset.value) >= span);
  return fit?.value ?? '7d';
}

// Only Prometheus panels render here; logs panels stay in Grafana.
function prometheusPanel(panel: GrafanaPanel): boolean {
  return panel.datasource?.type !== 'loki' && (panel.targets ?? []).every((t) => t.datasource?.type !== 'loki');
}

export function Monitor() {
  const [searchParams, setSearchParams] = useSearchParams();
  const range = searchParams.get('range') || DEFAULT_RANGE;
  const selectedWindow = range === 'custom' ? absoluteWindow(searchParams) : null;
  const selectedStart = selectedWindow?.start;
  const selectedEnd = selectedWindow?.end;
  const refreshSec = (() => {
    const raw = searchParams.get('refresh');
    if (raw == null) return DEFAULT_REFRESH;
    const n = parseInt(raw, 10);
    return Number.isFinite(n) && n >= 0 ? n : DEFAULT_REFRESH;
  })();

  const updateParams = useCallback(
    (patch: Record<string, string>) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          for (const [k, v] of Object.entries(patch)) {
            if (v === '') next.delete(k);
            else next.set(k, v);
          }
          return next;
        },
        { replace: true },
      );
    },
    [setSearchParams],
  );

  // Board list, then the selected board's panels.
  const [boards, setBoards] = useState<GrafanaDashboardSummary[] | null>(null);
  const [panels, setPanels] = useState<GrafanaPanel[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const board = searchParams.get('board') || boards?.[0]?.uid || '';

  useEffect(() => {
    let active = true;
    listDashboards()
      .then((list) => { if (active) setBoards(list); })
      .catch((cause) => { if (active) setError(cause instanceof ApiError ? cause.message : '看板列表加载失败'); });
    return () => { active = false; };
  }, []);

  useEffect(() => {
    if (!board) return;
    let active = true;
    setPanels(null);
    fetchDashboard(board)
      .then((resp) => {
        if (!active) return;
        setPanels(flattenPanels(resp.dashboard.panels ?? []).filter(prometheusPanel));
        setError(null);
      })
      .catch((cause) => { if (active) setError(cause instanceof ApiError ? cause.message : '看板加载失败'); });
    return () => { active = false; };
  }, [board]);

  const [tick, setTick] = useState(0);

  // Auto-refresh: bump tick → each PromQLPanel re-runs its query.
  // Paused when the tab is hidden; usePoll handles the visibility gate.
  // A fixed custom window has nothing new to fetch, so it doesn't poll.
  usePoll(() => setTick((t) => t + 1), refreshSec * 1000, refreshSec > 0 && range !== 'custom');

  // fromMs / toMs are computed off `range` and `tick` so a refresh slides
  // the window forward — the same effect Grafana's auto-refresh has.
  const { fromMs, toMs } = useMemo(() => {
    if (selectedStart && selectedEnd) return { fromMs: Date.parse(selectedStart), toMs: Date.parse(selectedEnd) };
    const now = Date.now();
    return { fromMs: now - rangeToMs(range), toMs: now };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range, tick, selectedStart, selectedEnd]);

  const queryRange = selectedStart && selectedEnd ? customRange(selectedStart, selectedEnd) : range;

  const handleRefresh = useCallback(() => {
    setTick((t) => t + 1);
  }, []);

  return (
    <>
      <PageHeader
        title="监控"
        description="sub2api、依赖、主机与平台自身的指标；与 Grafana 使用同一套看板定义。"
        actions={
          <Button onClick={handleRefresh} title="刷新所有图表">
            <RefreshCw size={13} aria-hidden="true" />
            刷新
          </Button>
        }
      >
        <div className="mt-3 flex flex-wrap items-center gap-2 text-[12px]">
          <TimeRangePicker
            value={{ range, start: selectedStart, end: selectedEnd }}
            presets={RANGE_PRESETS.map((option) => ({ value: option.value, label: option.label, durationMs: rangeToMs(option.value) }))}
            onChange={(selection) => {
              updateParams({ range: selection.range, start: selection.range === 'custom' ? selection.start : '', end: selection.range === 'custom' ? selection.end : '' });
              setTick((current) => current + 1);
            }}
          />
          <label className="flex items-center gap-1.5 whitespace-nowrap text-xs text-fg-muted">
            <RefreshCw size={12} aria-hidden="true" />
            自动刷新
            <SelectInput aria-label="自动刷新" value={String(refreshSec)} onChange={(event) => updateParams({ refresh: event.target.value })} className="w-24">
              {REFRESH_PRESETS.map((option) => <option key={option.value} value={String(option.value)}>{option.label}</option>)}
            </SelectInput>
          </label>
        </div>
      </PageHeader>
      <PageBody wide>
        {error && <Notice tone="danger">{error}</Notice>}
        {boards && boards.length > 0 && (
          <div className="rounded-lg border border-line bg-surface">
            <Tabs idBase="monitor" label="看板" value={board} onChange={(uid) => updateParams({ board: uid })}
              items={boards.map((item) => ({ key: item.uid, label: item.title }))} />
          </div>
        )}
        <div id={`monitor-panel-${board}`} role="tabpanel" aria-labelledby={`monitor-tab-${board}`}>
          {!panels ? (
            !error && <div className="grid gap-3 sm:grid-cols-2">{[0, 1, 2, 3].map((index) => <Skeleton key={index} className="h-48" />)}</div>
          ) : (
            <PanelGrid panels={panels} range={queryRange} fromMs={fromMs} toMs={toMs} tick={tick} />
          )}
        </div>
      </PageBody>
    </>
  );
}
