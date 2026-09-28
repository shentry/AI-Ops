// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/api/prom.ts — Copyright the ongrid authors, AGPL-3.0
// (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: calls this console's request helper
// at /api/v1/prometheus/query_range; lives at lib/prom.ts.

import { request } from '../api';

// Wire shape mirrors POST /api/v1/prometheus/query_range. The backend hands
// us the Prom matrix verbatim so the SPA can pivot however it needs —
// per-cpu, per-mountpoint, per-device, per-edge — without the server
// caring about the panel-specific reshaping.
//
// Why the matrix entries are typed as `[number, string]` rather than
// `[number, number]`: Prom serializes the value as a JSON string so it
// can carry +Inf / -Inf / NaN losslessly. Callers must `parseFloat` (and
// treat NaN/Inf as gaps if they want recharts to break the line).
export type PromMatrixSample = [number, string];
export type PromMatrixSeries = {
  metric: Record<string, string>;
  values: PromMatrixSample[];
};
export type PromQueryRangeResp = {
  result_type: 'matrix';
  result: PromMatrixSeries[];
  from: string;
  to: string;
};

export type PromQueryRangeInput = {
  expr: string;
  // RFC3339 strings — easier for the backend to parse than unix-ms.
  start: string;
  end: string;
  // Go duration string ("30s", "1m", "5m", ...).
  step: string;
};

// queryRange is the single PromQL entry point used by Monitor's native
// PromQLPanel. It runs through /api/v1/prometheus/query_range, which is
// gated by the console session (any signed-in viewer). The server applies
// a 30s timeout + 4 KiB expr cap; errors surface as ApiError so PromQLPanel
// can show inline red copy without taking down the whole grid.
export function queryRange(input: PromQueryRangeInput): Promise<PromQueryRangeResp> {
  return request<PromQueryRangeResp>('/api/v1/prometheus/query_range', { method: 'POST', body: JSON.stringify(input) });
}
