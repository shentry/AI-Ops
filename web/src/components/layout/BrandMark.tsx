import { cx } from "../ui";

// BrandMark：值班脉搏线。一个图形就够，不用渐变和发光。
export function BrandMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={cx("size-6 shrink-0", className)}>
      <rect width="24" height="24" rx="6" className="fill-accent" />
      <path d="M4 13.2h3.6l1.8-4.6 3 8.4 2.1-6 1.4 2.2H20" fill="none" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" className="stroke-accent-fg" />
    </svg>
  );
}
