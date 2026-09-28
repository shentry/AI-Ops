// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/components/ui/TimeRangePicker.tsx — Copyright the ongrid authors,
// AGPL-3.0 (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: the @base-ui Popover is replaced by
// this console's dismissable panel (outside click / Escape, as ModelMenu),
// controls use this console's Button / TextInput / FieldLabel, the file sits
// at components/TimeRangePicker.tsx, and strings are Chinese-only.

import { useEffect, useId, useRef, useState } from 'react';
import { ChevronDown, Clock } from 'lucide-react';
import { Button, FieldLabel, TextInput } from './ui';
import { localDateTime } from '../lib/telemetryContext';

export type TimeRangeSelection = { range: string; start: string; end: string };

type Props = {
  value: { range: string; start?: string; end?: string };
  presets: readonly { value: string; label: string; durationMs: number }[];
  onChange: (value: TimeRangeSelection) => void;
  minDurationMs?: number;
};

export function TimeRangePicker({ value, presets, onChange, minDurationMs = 1000 }: Props) {
  const id = useId();
  const root = useRef<HTMLDivElement | null>(null);
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState({ start: '', end: '' });
  const start = Date.parse(draft.start);
  const end = Date.parse(draft.end);
  const valid = Number.isFinite(start) && Number.isFinite(end) && end - start >= minDurationMs && end - start <= 7 * 86400000;
  const label = value.range === 'custom'
    ? '自定义时间'
    : presets.find((preset) => preset.value === value.range)?.label || '时间范围';
  const windowLabel = value.range === 'custom' && value.start && value.end
    ? `${localDateTime(value.start).replace('T', ' ')} — ${localDateTime(value.end).replace('T', ' ')}`
    : '';

  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', escape);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', escape);
    };
  }, [open]);

  const changeOpen = (next: boolean) => {
    if (next) {
      const now = Date.now();
      const duration = presets.find((preset) => preset.value === value.range)?.durationMs || 3600000;
      setDraft({
        start: localDateTime(value.start || new Date(now - duration).toISOString()),
        end: localDateTime(value.end || new Date(now).toISOString()),
      });
    }
    setOpen(next);
  };
  return (
    <div ref={root} className="relative">
      <Button aria-label="时间范围" aria-expanded={open} aria-haspopup="dialog" title={windowLabel || label}
        className="w-56 max-w-full justify-between" onClick={() => changeOpen(!open)}>
        <Clock size={13} aria-hidden="true" />
        <span className="min-w-0 flex-1 truncate text-left">{windowLabel ? `${localDateTime(value.start!).slice(5, 16).replace('T', ' ')} — ${localDateTime(value.end!).slice(5, 16).replace('T', ' ')}` : label}</span>
        <ChevronDown size={13} aria-hidden="true" />
      </Button>
      {open && (
        <div role="dialog" aria-label="时间范围" className="animate-fade-in absolute left-0 top-9 z-50 w-[32rem] max-w-[calc(100vw-1rem)] rounded-lg border border-line bg-surface shadow-xl shadow-black/25">
          <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-3">
            <p className="text-sm font-medium text-fg">时间范围</p>
            <span className="text-xs text-fg-muted">本地时区 · {Intl.DateTimeFormat().resolvedOptions().timeZone}</span>
          </div>
          <div className="grid sm:grid-cols-[9rem_minmax(0,1fr)]">
            <div className="border-b border-line p-3 sm:border-b-0 sm:border-r">
              <p className="mb-2 text-xs text-fg-muted">快捷范围</p>
              <div className="grid grid-cols-2 gap-1 sm:grid-cols-1">
                {presets.map((preset) => (
                  <Button key={preset.value} variant={value.range === preset.value ? 'secondary' : 'ghost'} size="sm" aria-pressed={value.range === preset.value} className="justify-start"
                    onClick={() => {
                      const now = Date.now();
                      onChange({ range: preset.value, start: new Date(now - preset.durationMs).toISOString(), end: new Date(now).toISOString() });
                      setOpen(false);
                    }}>
                    {preset.label}
                  </Button>
                ))}
              </div>
            </div>
            <form className="min-w-0 space-y-3 p-4" onSubmit={(event) => {
              event.preventDefault();
              event.stopPropagation();
              if (!valid) return;
              onChange({ range: 'custom', start: new Date(start).toISOString(), end: new Date(end).toISOString() });
              setOpen(false);
            }}>
              <p className="text-sm font-medium text-fg">自定义范围</p>
              {(['start', 'end'] as const).map((key) => (
                <div key={key}>
                  <FieldLabel htmlFor={`${id}-${key}`}>{key === 'start' ? '开始时间' : '结束时间'}</FieldLabel>
                  <TextInput id={`${id}-${key}`} type="datetime-local" step="1" value={draft[key]} required aria-invalid={!valid} aria-describedby={!valid ? `${id}-error` : undefined}
                    onChange={(event) => setDraft((current) => ({ ...current, [key]: event.target.value }))} className="w-full min-w-0" />
                </div>
              ))}
              {!valid && <p id={`${id}-error`} role="alert" className="text-xs text-danger">{minDurationMs >= 60000
                ? '请选择有效的起止时间，范围为 1 分钟至 7 天。'
                : '结束时间须晚于开始时间，范围不超过 7 天。'}</p>}
              <div className="flex justify-end gap-2 pt-1">
                <Button onClick={() => setOpen(false)}>取消</Button>
                <Button type="submit" variant="primary" disabled={!valid}>应用</Button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
}
