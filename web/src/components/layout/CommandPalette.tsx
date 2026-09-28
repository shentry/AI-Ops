import { CornerDownLeft, Search, Siren } from "lucide-react";
import { KeyboardEvent, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";

import { useIncidentFeed } from "../../app/context";
import { statusLabel } from "../../labels";
import { statusTone } from "../../tone";
import { Kbd, StatusDot, cx } from "../ui";
import { navGroups } from "./nav";

interface Entry {
  key: string;
  to: string;
  label: string;
  detail: string;
  haystack: string;
  kind: "page" | "incident";
  status?: string;
}

// CommandPalette 是 ⌘K / Ctrl+K 快速跳转：页面和最近事件，本地过滤。
export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { incidents } = useIncidentFeed();
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement | null>(null);
  const list = useRef<HTMLUListElement | null>(null);

  const entries = useMemo<Entry[]>(() => {
    const pages: Entry[] = navGroups.flatMap((group) => group.items.map((item) => ({
      key: `page:${item.to}`, to: item.to, label: item.label, detail: group.label, kind: "page" as const,
      haystack: `${item.label} ${group.label} ${item.keywords}`.toLowerCase(),
    })));
    const rows: Entry[] = incidents.map((incident) => ({
      key: `incident:${incident.id}`, to: `/incidents/${incident.id}`, kind: "incident" as const, status: incident.status,
      label: incident.title || `Incident #${incident.id}`, detail: `#${incident.id} · ${incident.group_key}`,
      haystack: `${incident.id} ${incident.title} ${incident.group_key}`.toLowerCase(),
    }));
    const needle = query.trim().toLowerCase();
    return [...pages, ...rows].filter((entry) => !needle || entry.haystack.includes(needle)).slice(0, 24);
  }, [incidents, query]);

  useEffect(() => {
    if (!open) return;
    setQuery("");
    setActive(0);
    window.setTimeout(() => input.current?.focus(), 0);
  }, [open]);

  useEffect(() => setActive(0), [query]);

  useEffect(() => {
    list.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active]);

  if (!open) return null;

  const go = (entry: Entry | undefined) => {
    if (!entry) return;
    onClose();
    navigate(entry.to);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActive((value) => Math.min(entries.length - 1, value + 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive((value) => Math.max(0, value - 1));
    } else if (event.key === "Enter" && !event.nativeEvent.isComposing) {
      event.preventDefault();
      go(entries[active]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      onClose();
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/45 px-4 pt-[14vh] backdrop-blur-[2px]" onMouseDown={onClose}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label="快速跳转"
        className="animate-fade-in w-full max-w-xl overflow-hidden rounded-xl border border-line bg-surface shadow-2xl shadow-black/40"
        onMouseDown={(event) => event.stopPropagation()}
      >
        <div className="flex items-center gap-2.5 border-b border-line px-4">
          <Search size={15} aria-hidden="true" className="text-fg-faint" />
          <input
            ref={input}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={onKeyDown}
            placeholder="搜索页面、事件标题或编号…"
            aria-label="搜索页面或事件"
            aria-controls="palette-results"
            aria-activedescendant={entries[active] ? `palette-${entries[active].key}` : undefined}
            className="h-12 min-w-0 flex-1 bg-transparent text-sm text-fg placeholder:text-fg-faint focus:outline-none"
          />
          <Kbd>Esc</Kbd>
        </div>
        <ul ref={list} id="palette-results" role="listbox" aria-label="跳转目标" className="max-h-80 overflow-y-auto p-1.5">
          {entries.length === 0 && <li className="px-3 py-6 text-center text-[13px] text-fg-faint">没有匹配的页面或事件</li>}
          {entries.map((entry, index) => (
            <li
              key={entry.key}
              id={`palette-${entry.key}`}
              role="option"
              aria-selected={index === active}
              data-index={index}
              onMouseEnter={() => setActive(index)}
              onClick={() => go(entry)}
              className={cx("flex cursor-pointer items-center gap-3 rounded-md px-3 py-2 text-[13px]", index === active ? "bg-surface-2 text-fg" : "text-fg-muted")}
            >
              {entry.kind === "incident"
                ? <StatusDot tone={statusTone(entry.status)} />
                : <span className="w-2" aria-hidden="true" />}
              <span className="min-w-0 flex-1 truncate">{entry.label}</span>
              <span className="shrink-0 text-xs text-fg-faint">
                {entry.kind === "incident" ? `${statusLabel(entry.status)} · ${entry.detail}` : entry.detail}
              </span>
              {index === active && <CornerDownLeft size={13} aria-hidden="true" className="shrink-0 text-fg-faint" />}
            </li>
          ))}
        </ul>
        <div className="flex items-center gap-3 border-t border-line-soft px-4 py-2 text-[11px] text-fg-faint">
          <Siren size={12} aria-hidden="true" />
          <span>最近 50 个事件可搜索；更早的请到事件列表按状态筛选。</span>
        </div>
      </div>
    </div>
  );
}
