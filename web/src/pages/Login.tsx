import { KeyRound, ShieldCheck } from "lucide-react";
import { FormEvent, useState } from "react";

import { ApiError, Session, login } from "../api";
import { BrandMark } from "../components/layout/BrandMark";
import { Button, FieldLabel, TextInput } from "../components/ui";

// Login 用个人令牌换取 HttpOnly 会话 Cookie。没有匿名控制台：
// 每个操作都记在服务端证明过的身份名下；令牌不写入任何浏览器存储。
export function Login({ onLogin }: { onLogin: (session: Session) => void }) {
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!token.trim() || busy) return;
    setBusy(true);
    setError(null);
    try {
      onLogin(await login(token));
    } catch (cause) {
      setError(cause instanceof ApiError && cause.status === 401 ? "令牌无效" : "登录失败，请稍后重试");
    } finally {
      setToken("");
      setBusy(false);
    }
  };

  return (
    <main className="grid min-h-dvh place-items-center bg-canvas px-5">
      <div className="w-full max-w-[380px]">
        <div className="mb-6 flex items-center gap-2.5">
          <BrandMark className="size-8" />
          <div>
            <p className="text-[15px] font-semibold leading-tight">值班控制台</p>
            <p className="text-xs text-fg-faint">oncall-agent · AI-Ops</p>
          </div>
        </div>
        <form onSubmit={submit} className="rounded-xl border border-line bg-surface p-5 shadow-2xl shadow-black/20">
          <h1 className="flex items-center gap-2 text-[15px] font-semibold"><KeyRound size={15} aria-hidden="true" className="text-accent" />登录</h1>
          <p className="mt-1 text-[13px] text-fg-muted">用管理员发放的个人令牌登录，会话 12 小时后过期。</p>
          <div className="mt-4">
            <FieldLabel htmlFor="operator-token">个人令牌</FieldLabel>
            <TextInput
              id="operator-token"
              type="password"
              autoComplete="off"
              autoFocus
              value={token}
              disabled={busy}
              onChange={(event) => setToken(event.target.value)}
              placeholder="不会保存在浏览器"
              className="h-9 font-mono"
            />
          </div>
          {error && <p role="alert" className="mt-2.5 rounded-md border border-danger/35 bg-danger/10 px-2.5 py-1.5 text-[13px] text-danger">{error}</p>}
          <Button type="submit" variant="primary" className="mt-4 h-9 w-full" disabled={busy || !token.trim()}>
            {busy ? "登录中…" : "登录"}
          </Button>
        </form>
        <p className="mt-4 flex items-start gap-1.5 text-xs text-fg-faint">
          <ShieldCheck size={13} aria-hidden="true" className="mt-px shrink-0" />
          审批、急停和复盘都按登录身份记录；请求头里的操作人字段不作为身份。
        </p>
      </div>
    </main>
  );
}
