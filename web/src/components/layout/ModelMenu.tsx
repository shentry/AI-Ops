import { Check, ChevronDown, Cpu } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { ApiError, switchCurrentModel } from "../../api";
import { useSession } from "../../app/context";
import { cx } from "../ui";

// ModelMenu shows the diagnosis model to everyone signed in. Only admins may
// switch it; the server checks the session role again on PUT.
export function ModelMenu() {
  const { session, model, setModel } = useSession();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const root = useRef<HTMLDivElement | null>(null);
  const canSwitch = session.role === "admin" && (model?.models.length ?? 0) > 1;

  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", escape);
    };
  }, [open]);

  if (!model?.current_model) return null;

  const choose = async (id: string) => {
    if (busy || id === model.current_model) {
      setOpen(false);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      setModel(await switchCurrentModel(id));
      setOpen(false);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "模型切换失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div ref={root} className="relative min-w-0 flex-1">
      <button
        type="button"
        disabled={!canSwitch}
        aria-haspopup={canSwitch ? "menu" : undefined}
        aria-expanded={canSwitch ? open : undefined}
        title={canSwitch ? "切换诊断模型" : "当前诊断模型"}
        onClick={() => setOpen((value) => !value)}
        className="flex h-8 w-full min-w-0 cursor-pointer items-center gap-2 rounded-md px-2 text-left text-xs text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg disabled:cursor-default disabled:hover:bg-transparent disabled:hover:text-fg-muted"
      >
        <Cpu size={14} aria-hidden="true" className="shrink-0" />
        <span className="min-w-0 flex-1 truncate font-mono">{model.current_model}</span>
        {canSwitch && <ChevronDown size={13} aria-hidden="true" className="shrink-0" />}
      </button>
      {open && (
        <div role="menu" aria-label="诊断模型" className="animate-fade-in absolute bottom-10 left-0 z-50 w-64 rounded-lg border border-line bg-surface p-1 shadow-xl shadow-black/25">
          <p className="px-2 pb-1 pt-1.5 text-[11px] text-fg-faint">切换只影响后续诊断和新提问，运行中的任务继续用原模型。</p>
          {model.models.map((option) => (
            <button
              key={option.id}
              type="button"
              role="menuitemradio"
              aria-checked={option.id === model.current_model}
              disabled={busy}
              onClick={() => void choose(option.id)}
              className="flex w-full cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs text-fg hover:bg-surface-2 disabled:opacity-50"
            >
              <span className="min-w-0 flex-1 truncate font-mono">{option.id}</span>
              {option.thinking_enabled && <span className="text-[10.5px] text-fg-faint">thinking</span>}
              <Check size={13} aria-hidden="true" className={cx(option.id === model.current_model ? "text-accent" : "invisible")} />
            </button>
          ))}
          {error && <p role="alert" className="px-2 py-1.5 text-xs text-danger">{error}</p>}
        </div>
      )}
    </div>
  );
}
