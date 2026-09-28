import { Link } from "react-router";

import { buttonClass } from "../components/ui";

export function NotFound() {
  return (
    <div className="grid min-h-[60vh] place-items-center px-6">
      <div className="max-w-sm text-center">
        <p className="font-mono text-xs text-fg-faint">404</p>
        <h1 className="mt-1 text-[16px] font-semibold">没有这个页面</h1>
        <p className="mt-1 text-[13px] text-fg-muted">链接可能已失效，或者这个 Incident 编号不存在。</p>
        <Link to="/" className={buttonClass("secondary", "md", "mt-4")}>回到概览</Link>
      </div>
    </div>
  );
}
