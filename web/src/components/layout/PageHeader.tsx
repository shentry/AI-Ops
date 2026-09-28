import { ArrowLeft } from "lucide-react";
import { ReactNode } from "react";
import { Link } from "react-router";

// PageHeader 固定在主滚动区顶部：返回链接、标题、徽标/元信息与页面动作。
export function PageHeader({ title, description, back, badges, meta, actions, children }: {
  title: string;
  description?: ReactNode;
  back?: { to: string; label: string };
  badges?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <header className="z-20 border-b border-line bg-canvas/88 px-5 pb-3 pt-3.5 backdrop-blur-md sm:px-7 lg:sticky lg:top-0">
      {back && (
        <Link to={back.to} className="mb-1 inline-flex items-center gap-1 text-xs text-fg-muted hover:text-fg">
          <ArrowLeft size={12} aria-hidden="true" />
          {back.label}
        </Link>
      )}
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="min-w-0 flex-1 basis-80">
          <h1 className="truncate text-[17px] font-semibold tracking-tight text-fg">{title}</h1>
          {description && <p className="mt-0.5 text-[13px] text-fg-muted">{description}</p>}
          {badges && <div className="mt-1.5 flex flex-wrap items-center gap-1.5">{badges}</div>}
          {meta && <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-fg-faint">{meta}</div>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </header>
  );
}

export function PageBody({ children, wide = false }: { children: ReactNode; wide?: boolean }) {
  return <div className={wide ? "space-y-4 px-5 py-5 sm:px-7" : "mx-auto max-w-6xl space-y-4 px-5 py-5 sm:px-7"}>{children}</div>;
}
