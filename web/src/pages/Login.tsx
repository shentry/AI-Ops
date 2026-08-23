// Legacy OAuth fallback kept for compatibility; the public console does not import it.
interface LoginProps {
  error?: string | null;
}

export function Login({ error }: LoginProps) {
  return (
    <main className="shell login-shell">
      <div className="login-orbit orbit-one" />
      <div className="login-orbit orbit-two" />
      <section className="login-card" aria-labelledby="login-title">
        <div className="brand login-brand">
          <span className="brand-mark">O</span>
          <span>值班 <b>控制台</b></span>
        </div>
        <span className="eyebrow">故障处置作战台</span>
        <h1 id="login-title">把每条信号<br /><em>都盯住。</em></h1>
        <p className="login-copy">
          诊断、审批、执行、验证共用同一份状态，与飞书机器人完全一致。
          用飞书账号登录后继续。
        </p>
        {error && <div className="inline-error" role="alert">{error}</div>}
        <button className="button feishu-button" type="button" onClick={() => {
          const next = `${window.location.pathname}${window.location.search}${window.location.hash}`;
          window.location.assign(`/auth/feishu/start?next=${encodeURIComponent(next)}`);
        }}>
          <span className="feishu-glyph" aria-hidden="true">↗</span>
          用飞书登录
        </button>
        <p className="login-note">只有审批白名单里的人可以执行变更。</p>
      </section>
      <footer className="login-footer">所有事件已落库 · 所有动作可审计</footer>
    </main>
  );
}
