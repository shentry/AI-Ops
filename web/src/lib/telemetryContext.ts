// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/lib/telemetryContext.ts — Copyright the ongrid authors, AGPL-3.0
// (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: kept only the time-window helpers the
// Monitor page uses; the log/trace correlation helpers were not copied.

export function absoluteWindow(params: URLSearchParams) {
  const start = params.get('start') || '';
  const end = params.get('end') || '';
  const a = Date.parse(start);
  const b = Date.parse(end);
  return Number.isFinite(a) && Number.isFinite(b) && b > a && b - a <= 7 * 86400000
    ? { start: new Date(a).toISOString(), end: new Date(b).toISOString() }
    : null;
}

export function localDateTime(value: string) {
  const date = new Date(value);
  return Number.isFinite(date.getTime())
    ? new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 19)
    : '';
}
