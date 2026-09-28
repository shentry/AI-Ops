import { KeyboardEvent, ReactNode, forwardRef, useId } from "react";
import type { ButtonHTMLAttributes, InputHTMLAttributes, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";

import { Tone, toneDot } from "../tone";

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "ok";
type ButtonSize = "xs" | "sm" | "md";

const buttonVariants: Record<ButtonVariant, string> = {
  primary: "border-transparent bg-accent text-accent-fg hover:bg-accent/88",
  secondary: "border-line bg-surface text-fg hover:border-fg-faint/50 hover:bg-surface-2",
  ghost: "border-transparent text-fg-muted hover:bg-surface-2 hover:text-fg",
  danger: "border-danger/40 bg-danger/12 text-danger hover:bg-danger/20",
  ok: "border-ok/40 bg-ok/12 text-ok hover:bg-ok/20",
};

const buttonSizes: Record<ButtonSize, string> = {
  xs: "h-6 gap-1 px-2 text-[11.5px]",
  sm: "h-7 gap-1.5 px-2.5 text-xs",
  md: "h-8 gap-2 px-3 text-[13px]",
};

export function buttonClass(variant: ButtonVariant = "secondary", size: ButtonSize = "md", className?: string): string {
  return cx(
    "inline-flex shrink-0 cursor-pointer items-center justify-center whitespace-nowrap rounded-md border font-medium transition-colors",
    "disabled:pointer-events-none disabled:opacity-45",
    buttonVariants[variant],
    buttonSizes[size],
    className,
  );
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", size = "md", className, type = "button", ...rest },
  ref,
) {
  return <button ref={ref} type={type} className={buttonClass(variant, size, className)} {...rest} />;
});

const badgeTones: Record<Tone, string> = {
  neutral: "border-line bg-surface-2 text-fg-muted",
  accent: "border-accent/35 bg-accent/12 text-accent",
  ok: "border-ok/35 bg-ok/12 text-ok",
  warn: "border-warn/35 bg-warn/12 text-warn",
  danger: "border-danger/35 bg-danger/12 text-danger",
  info: "border-info/35 bg-info/12 text-info",
};

export function Badge({ tone = "neutral", dot = false, pulse = false, title, className, children }: {
  tone?: Tone;
  dot?: boolean;
  pulse?: boolean;
  title?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <span title={title} className={cx("inline-flex max-w-full items-center gap-1.5 whitespace-nowrap rounded-md border px-1.5 py-px text-[11px] font-medium leading-[18px]", badgeTones[tone], className)}>
      {dot && <span aria-hidden="true" className={cx("size-1.5 shrink-0 rounded-full bg-current", pulse && "animate-pulse-dot")} />}
      {children}
    </span>
  );
}

export function StatusDot({ tone, pulse = false, className }: { tone: Tone; pulse?: boolean; className?: string }) {
  return <span aria-hidden="true" className={cx("inline-block size-2 shrink-0 rounded-full", toneDot[tone], pulse && "animate-pulse-dot", className)} />;
}

// Panel 是带标题的卡片。标题 h2 通过 aria-labelledby 成为区域名，
// 测试和读屏都按这个名字找到它（例如「等待你审批」「最近变更」）。
export function Panel({ title, icon, meta, actions, tone, className, bodyClassName, children }: {
  title: string;
  icon?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  tone?: "warn" | "danger" | "accent";
  className?: string;
  bodyClassName?: string;
  children: ReactNode;
}) {
  const id = useId();
  const border = tone === "warn" ? "border-warn/45" : tone === "danger" ? "border-danger/45" : tone === "accent" ? "border-accent/45" : "border-line";
  return (
    <section aria-labelledby={id} className={cx("min-w-0 rounded-lg border bg-surface", border, className)}>
      <header className="flex min-h-11 items-center gap-2 border-b border-line-soft px-4 py-2">
        {icon && <span aria-hidden="true" className="text-fg-faint">{icon}</span>}
        <h2 id={id} className="text-[13px] font-semibold text-fg">{title}</h2>
        {meta && <div className="flex min-w-0 items-center gap-2 text-xs text-fg-faint">{meta}</div>}
        {actions && <div className="ml-auto flex items-center gap-1.5">{actions}</div>}
      </header>
      <div className={cx("px-4 py-3", bodyClassName)}>{children}</div>
    </section>
  );
}

export function EmptyState({ icon, title, hint, className }: { icon?: ReactNode; title: string; hint?: ReactNode; className?: string }) {
  return (
    <div className={cx("flex items-start gap-3 py-2", className)}>
      {icon && <span aria-hidden="true" className="mt-0.5 text-fg-faint">{icon}</span>}
      <div className="min-w-0">
        <p className="text-[13px] text-fg-muted">{title}</p>
        {hint && <p className="mt-0.5 text-xs text-fg-faint">{hint}</p>}
      </div>
    </div>
  );
}

// Facts 是紧凑的键值列表，审批快照、执行回执等「事实」都用它展示。
export function Facts({ children, className }: { children: ReactNode; className?: string }) {
  return <dl className={cx("grid grid-cols-[88px_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-[12.5px]", className)}>{children}</dl>;
}

export function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-fg-faint">{label}</dt>
      <dd className="min-w-0 break-words text-fg">{children}</dd>
    </>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <code className={cx("font-mono text-[12px] text-fg break-all", className)}>{children}</code>;
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded border border-line bg-surface-2 px-1 font-mono text-[10.5px] leading-4 text-fg-faint">{children}</kbd>;
}

const fieldBase = "w-full rounded-md border border-line bg-canvas px-2.5 text-[13px] text-fg placeholder:text-fg-faint transition-colors focus:border-accent/60 focus:outline-none focus:ring-2 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-55";

export function FieldLabel({ htmlFor, children, hint }: { htmlFor: string; children: ReactNode; hint?: ReactNode }) {
  return (
    <label htmlFor={htmlFor} className="mb-1 block text-xs font-medium text-fg-muted">
      {children}{hint && <span className="font-normal text-fg-faint">{hint}</span>}
    </label>
  );
}

export const TextInput = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function TextInput({ className, ...rest }, ref) {
  return <input ref={ref} className={cx(fieldBase, "h-8", className)} {...rest} />;
});

export const TextArea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(function TextArea({ className, ...rest }, ref) {
  return <textarea ref={ref} className={cx(fieldBase, "py-1.5 leading-relaxed", className)} {...rest} />;
});

export function SelectInput({ className, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cx(fieldBase, "h-8 cursor-pointer pr-7", className)} {...rest} />;
}

// Notice 是页面级的反馈条。role=status 不打断读屏；只有必须阻止操作的
// 信息（例如审批快照不完整）才用 role=alert，且在组件内部单独给出。
export function Notice({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <div role="status" className={cx("rounded-md border px-3 py-2 text-[13px]", badgeTones[tone])}>
      {children}
    </div>
  );
}

export interface TabItem<K extends string> {
  key: K;
  label: string;
  count?: number;
}

// Tabs 遵循 WAI-ARIA tabs 模式：方向键在标签间移动，面板用 aria-controls 关联。
export function Tabs<K extends string>({ items, value, onChange, idBase, label }: {
  items: TabItem<K>[];
  value: K;
  onChange: (key: K) => void;
  idBase: string;
  label: string;
}) {
  const move = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
    event.preventDefault();
    const next = items[(index + (event.key === "ArrowRight" ? 1 : items.length - 1)) % items.length];
    onChange(next.key);
    document.getElementById(`${idBase}-tab-${next.key}`)?.focus();
  };
  return (
    <div role="tablist" aria-label={label} className="flex items-center gap-1 border-b border-line-soft px-2">
      {items.map((item, index) => {
        const selected = item.key === value;
        return (
          <button
            key={item.key}
            id={`${idBase}-tab-${item.key}`}
            type="button"
            role="tab"
            aria-selected={selected}
            aria-controls={`${idBase}-panel-${item.key}`}
            tabIndex={selected ? 0 : -1}
            onClick={() => onChange(item.key)}
            onKeyDown={(event) => move(event, index)}
            className={cx(
              "relative -mb-px flex h-10 cursor-pointer items-center gap-1.5 border-b-2 px-2.5 text-[13px] transition-colors",
              selected ? "border-accent font-medium text-fg" : "border-transparent text-fg-muted hover:text-fg",
            )}
          >
            {item.label}
            {item.count !== undefined && (
              <span className={cx("tabular rounded px-1 text-[11px]", selected ? "bg-accent/15 text-accent" : "bg-surface-2 text-fg-faint")}>{item.count}</span>
            )}
          </button>
        );
      })}
    </div>
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden="true" className={cx("animate-pulse rounded-md bg-surface-2", className)} />;
}
