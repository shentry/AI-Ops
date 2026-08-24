import { Cpu, X } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiError, ModelState, switchCurrentModel } from "../api";

interface ModelSwitcherProps {
  state: ModelState | null;
  onChanged: (state: ModelState) => void;
}

// The control room is public, but switching the process-wide model requires a
// management Bearer token. The token stays only in this component's memory.
export function ModelSwitcher({ state, onChanged }: ModelSwitcherProps) {
  const [open, setOpen] = useState(false);
  const [selectedModel, setSelectedModel] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setSelectedModel(state?.current_model ?? "");
  }, [state?.current_model]);

  if (!state || !state.current_model) return null;

  const submit = async () => {
    if (!selectedModel || busy) return;
    setBusy(true);
    setError(null);
    try {
      const next = await switchCurrentModel(token, selectedModel);
      onChanged(next);
      setToken("");
      setOpen(false);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "模型切换失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="model-switcher">
      <button
        className="model-switcher-trigger"
        type="button"
        aria-expanded={open}
        onClick={() => {
          setOpen((value) => !value);
          setError(null);
        }}
      >
        <Cpu size={12} />
        <strong>{state.current_model}</strong>
      </button>
      {open && (
        <section className="model-switcher-popover" aria-label="模型管理">
          <div className="model-switcher-heading">
            <div>
              <span className="eyebrow">LLM 路由</span>
              <strong>切换诊断模型</strong>
            </div>
            <button className="button subtle" type="button" aria-label="关闭" onClick={() => setOpen(false)}><X size={13} /></button>
          </div>
          <p>切换只影响后续诊断和新提问；正在执行的任务继续使用原模型。</p>
          <label className="field-label" htmlFor="selected-model">候选模型</label>
          <select
            id="selected-model"
            className="text-input model-select"
            value={selectedModel}
            disabled={busy}
            onChange={(event) => setSelectedModel(event.target.value)}
          >
            {state.models.map((model) => (
              <option key={model.id} value={model.id}>
                {model.id}{model.thinking_enabled ? " · thinking" : ""}
              </option>
            ))}
          </select>
          <label className="field-label" htmlFor="model-management-token">模型管理令牌</label>
          <input
            id="model-management-token"
            className="text-input"
            type="password"
            autoComplete="off"
            value={token}
            disabled={busy}
            onChange={(event) => setToken(event.target.value)}
            placeholder="输入 AUTH_TOKEN，仅本次请求使用"
          />
          {error && <div className="inline-error model-switcher-error" role="alert">{error}</div>}
          <button className="button primary" type="button" disabled={busy || !token.trim() || !selectedModel} onClick={() => void submit()}>
            {busy ? "切换中…" : "切换模型"}
          </button>
        </section>
      )}
    </div>
  );
}
