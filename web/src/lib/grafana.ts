// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/api/grafana.ts — Copyright the ongrid authors, AGPL-3.0
// (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: the dashboards come from the
// server's embedded catalog (internal/grafana) rather than a Grafana proxy;
// added listDashboards for the board tabs and the panel-level datasource;
// lives at lib/grafana.ts.

import { request } from '../api';

// Grafana dashboard JSON, walked at runtime by Monitor.tsx /
// PromQLPanel.tsx to render panels natively (no iframe).
//
// We model only the fields the renderer actually uses. Grafana's full
// dashboard schema is enormous and changes every minor release — keep
// the SPA loose so Grafana 10 / 11 / 12 panels all decode without the
// SPA needing per-version branches. Unknown fields are ignored.

// GrafanaThreshold is one step in `fieldConfig.defaults.thresholds.steps`.
// `value` may be null for the lowest band ("from -∞").
export type GrafanaThreshold = {
  color: string;
  value: number | null;
};

export type GrafanaThresholdsConfig = {
  mode: 'absolute' | 'percentage' | string;
  steps: GrafanaThreshold[];
};

export type GrafanaFieldConfig = {
  defaults?: {
    unit?: string;
    min?: number;
    max?: number;
    decimals?: number;
    thresholds?: GrafanaThresholdsConfig;
    [k: string]: unknown;
  };
  overrides?: unknown[];
};

export type GrafanaTarget = {
  expr?: string;
  refId?: string;
  legendFormat?: string;
  // Some panel types (e.g. logs) carry a Loki/Tempo query in `expr`
  // alongside a `datasource.type` field; we don't render those natively
  // but pass them through so the panel knows to fall back to deep-link.
  datasource?: { type?: string; uid?: string } | null;
};

export type GrafanaPanel = {
  id: number;
  type: string;
  title: string;
  description?: string;
  gridPos: { x: number; y: number; w: number; h: number };
  targets?: GrafanaTarget[];
  fieldConfig?: GrafanaFieldConfig;
  options?: Record<string, unknown>;
  // Panel-level datasource; Monitor skips Loki (logs) panels.
  datasource?: { type?: string; uid?: string } | null;
  // Row panels carry nested panels[]. We flatten before rendering — the
  // PromQLPanel renderer never sees a row.
  panels?: GrafanaPanel[];
  collapsed?: boolean;
};

export type GrafanaDashboard = {
  uid: string;
  title: string;
  panels: GrafanaPanel[];
  // templating.list[] holds dashboard variables ($device_id, $datasource).
  // We don't render the picker UI in v1; lib/grafanaVars.ts substitutes
  // a small allowlist directly from URL query params.
  templating?: { list?: Array<{ name: string; current?: { value?: unknown } }> };
};

export type GrafanaDashboardResp = {
  dashboard: GrafanaDashboard;
  meta?: Record<string, unknown>;
};

// fetchDashboard reads one dashboard JSON — the same file Grafana provisions —
// so the console renders exactly the panels Grafana shows.
export async function fetchDashboard(uid: string): Promise<GrafanaDashboardResp> {
  return request<GrafanaDashboardResp>(`/api/v1/observability/dashboards/${encodeURIComponent(uid)}`);
}

export type GrafanaDashboardSummary = { uid: string; title: string };

export async function listDashboards(): Promise<GrafanaDashboardSummary[]> {
  const body = await request<{ dashboards: GrafanaDashboardSummary[] }>('/api/v1/observability/dashboards');
  return body.dashboards ?? [];
}
