import { useEffect, useState } from "react";

import { ApiError, Incident, ModelState, listIncidents } from "../api";
import { ModelSwitcher } from "../components/ModelSwitcher";
import { severityLabel, statusLabel, timeLabel } from "../labels";

interface IncidentPickerProps {
  actorName?: string;
  modelState: ModelState | null;
  onModelChanged: (state: ModelState) => void;
}

// IncidentPicker is the public console entry point.
export function IncidentPicker({ actorName, modelState, onModelChanged }: IncidentPickerProps) {
  const [incidents, setIncidents] = useState<Incident[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    void listIncidents()
      .then((rows) => {
        if (!active) return;
        setIncidents(rows);
        setError(null);
      })
      .catch((cause) => {
        if (!active) return;
        setIncidents([]);
        setError(cause instanceof ApiError ? cause.message : "Incident 列表加载失败。");
      });
    return () => {
      active = false;
    };
  }, []);

  return (
    <main className="shell room-shell">
      <header className="topbar">
        <a className="brand" href="/" aria-label="值班控制台首页">
          <span className="brand-mark">O</span>
          <span>值班 <b>控制台</b></span>
        </a>
        <div className="topbar-actions">
          <ModelSwitcher state={modelState} onChanged={onModelChanged} />
          {actorName && <span className="operator">{actorName}</span>}
        </div>
      </header>

      <section className="incident-heading">
        <div>
          <div className="breadcrumb">作战台</div>
          <h1>Incident 列表</h1>
          <div className="incident-meta">
            <span>{incidents === null ? "加载中…" : `最近 ${incidents.length} 条`}</span>
            <span>点进去看诊断链路、审批和 Agent 对话</span>
          </div>
        </div>
      </section>

      {error && <div className="inline-error" role="status">{error}</div>}

      <section className="panel">
        <div className="panel-heading">
          <h2>选一个 Incident</h2>
          <span className="panel-caption">最新的在最前面</span>
        </div>
        {incidents !== null && incidents.length === 0 ? (
          <div className="empty-state compact-empty">
            <span className="empty-check">✓</span>
            <p>还没有任何 Incident</p>
            <small>Alertmanager 还没有推送过告警，或者都被去重掉了。</small>
          </div>
        ) : (
          <ul className="picker-list">
            {(incidents ?? []).map((incident) => (
              <li key={incident.id}>
                <a className="picker-item" href={`/incidents/${incident.id}`}>
                  <span className={`severity severity-${String(incident.severity).toLowerCase()}`}>
                    {severityLabel(incident.severity)}
                  </span>
                  <span className="picker-title">{incident.title || `Incident #${incident.id}`}</span>
                  <span className="status-pill">{statusLabel(incident.status)}</span>
                  <span className="picker-meta">
                    {incident.alerts_count} 条告警 · {timeLabel(incident.last_seen_at)}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </section>
    </main>
  );
}
