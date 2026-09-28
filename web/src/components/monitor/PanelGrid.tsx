// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/components/monitor/PanelGrid.tsx — Copyright the ongrid authors,
// AGPL-3.0 (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: dropped the Grafana deep-link props;
// imports follow this console's layout.

// PanelGrid — lays out PromQL-driven panels on Grafana's 24-column grid.
// Each panel's gridPos.{x,y,w,h} maps directly to CSS grid coordinates;
// we don't honor `y` strictly because CSS grid auto-flow handles
// vertical stacking and panels are rarely sparse-on-Y in practice.
// `h` is in Grafana grid units (~30px each); scaled to ~36px so the
// embedded view feels comfortable in our denser SPA chrome.
//
// On screens narrower than ~1200px we collapse to a single column so
// each panel stays readable on laptops/tablets — Grafana JSON's tight
// 6-col widths become unreadable below that.
import { useEffect, useState } from 'react';
import { type GrafanaPanel } from '../../lib/grafana';
import { PromQLPanel } from '../PromQLPanel';

export function PanelGrid({
  panels,
  range,
  fromMs,
  toMs,
  tick,
}: {
  panels: GrafanaPanel[];
  range: string;
  fromMs: number;
  toMs: number;
  tick: number;
}) {
  const [narrow, setNarrow] = useState<boolean>(
    typeof window !== 'undefined' ? window.innerWidth < 1200 : false,
  );
  useEffect(() => {
    const onResize = () => setNarrow(window.innerWidth < 1200);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);

  return (
    <div
      className="grid gap-3"
      style={{
        gridTemplateColumns: narrow ? '1fr' : 'repeat(24, minmax(0, 1fr))',
        gridAutoRows: '36px',
      }}
    >
      {panels.map((p) => {
        const w = Math.max(1, Math.min(24, p.gridPos?.w ?? 12));
        const h = Math.max(4, p.gridPos?.h ?? 8);
        const style = narrow
          ? { gridColumn: '1 / -1', gridRow: `span ${Math.max(6, h)}`, minHeight: 220 }
          : { gridColumn: `span ${w}`, gridRow: `span ${h}`, minHeight: 180 };
        return (
          <div key={p.id} style={style} className="min-w-0">
            <PromQLPanel
              panel={p}
              range={range}
              tick={tick}
              fromMs={fromMs}
              toMs={toMs}
            />
          </div>
        );
      })}
    </div>
  );
}
