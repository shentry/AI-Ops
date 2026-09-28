import { Menu } from "lucide-react";
import { useEffect, useState } from "react";
import { Outlet, useLocation } from "react-router";

import { cx } from "../ui";
import { BrandMark } from "./BrandMark";
import { CommandPalette } from "./CommandPalette";
import { Sidebar } from "./Sidebar";

// AppLayout：左侧导航 + 右侧独立滚动的主区域。窄屏时导航收成抽屉。
export function AppLayout() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [navOpen, setNavOpen] = useState(false);
  const location = useLocation();

  useEffect(() => setNavOpen(false), [location.pathname]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen((value) => !value);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="flex h-dvh overflow-hidden">
      <div
        className={cx(
          "fixed inset-y-0 left-0 z-40 transition-transform duration-200 lg:static lg:translate-x-0",
          navOpen ? "translate-x-0" : "-translate-x-full",
        )}
      >
        <Sidebar onSearch={() => setPaletteOpen(true)} onClose={() => setNavOpen(false)} />
      </div>
      {navOpen && <div aria-hidden="true" className="fixed inset-0 z-30 bg-black/40 lg:hidden" onClick={() => setNavOpen(false)} />}

      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex h-12 shrink-0 items-center gap-3 border-b border-line bg-sidebar px-4 lg:hidden">
          <button type="button" aria-label="打开导航" onClick={() => setNavOpen(true)} className="rounded-md p-1.5 text-fg-muted hover:bg-surface-2">
            <Menu size={17} aria-hidden="true" />
          </button>
          <BrandMark className="size-5" />
          <span className="text-[13px] font-semibold">值班控制台</span>
        </div>
        <main id="main" className="min-h-0 flex-1 overflow-y-auto">
          <Outlet />
        </main>
      </div>

      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
    </div>
  );
}
