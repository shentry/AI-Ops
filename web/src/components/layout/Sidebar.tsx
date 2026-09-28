import { LogOut, Moon, Search, Sun, X } from "lucide-react";
import { useState } from "react";
import { Link, NavLink } from "react-router";

import { useIncidentFeed, useSession } from "../../app/context";
import { Theme, applyTheme, readTheme } from "../../app/theme";
import { statusTone } from "../../tone";
import { Kbd, StatusDot, cx } from "../ui";
import { BrandMark } from "./BrandMark";
import { ModelMenu } from "./ModelMenu";
import { navGroups } from "./nav";

const roleNames = { viewer: "只读", operator: "值班", admin: "管理员" } as const;

export function Sidebar({ onSearch, onClose }: { onSearch: () => void; onClose?: () => void }) {
  const { session, signOut } = useSession();
  const { incidents, firing } = useIncidentFeed();
  const [theme, setTheme] = useState<Theme>(readTheme);
  const recent = incidents.slice(0, 6);
  const display = session.name || session.id;

  const toggleTheme = () => {
    const next: Theme = theme === "dark" ? "light" : "dark";
    applyTheme(next, true);
    setTheme(next);
  };

  return (
    <nav aria-label="主导航" className="flex h-full w-64 flex-col border-r border-line bg-sidebar">
      <div className="flex h-14 shrink-0 items-center gap-2.5 px-4">
        <BrandMark />
        <Link to="/" className="min-w-0 flex-1">
          <span className="block text-[14px] font-semibold leading-tight text-fg">值班控制台</span>
          <span className="block text-[11px] leading-tight text-fg-faint">oncall-agent · AI-Ops</span>
        </Link>
        {onClose && (
          <button type="button" aria-label="关闭导航" onClick={onClose} className="rounded-md p-1 text-fg-muted hover:bg-surface-2 lg:hidden">
            <X size={16} aria-hidden="true" />
          </button>
        )}
      </div>

      <div role="group" aria-label={`当前用户：${display}（${roleNames[session.role]}）`} className="mx-3 flex items-center gap-2.5 rounded-lg border border-line-soft bg-surface/60 px-2.5 py-2">
        <span aria-hidden="true" className="grid size-8 shrink-0 place-items-center rounded-full bg-accent/15 text-[13px] font-semibold text-accent">
          {display.slice(0, 1).toUpperCase()}
        </span>
        <div className="min-w-0 flex-1">
          <p className="truncate text-[13px] font-medium text-fg">{display}</p>
          <p className="text-[11px] text-fg-faint">{roleNames[session.role]}</p>
        </div>
        <button type="button" aria-label="退出登录" title="退出登录" onClick={signOut} className="rounded-md p-1.5 text-fg-faint hover:bg-surface-2 hover:text-fg">
          <LogOut size={14} aria-hidden="true" />
        </button>
      </div>

      <button
        type="button"
        onClick={onSearch}
        className="mx-3 mt-2.5 flex h-8 cursor-pointer items-center gap-2 rounded-md border border-line-soft bg-canvas/50 px-2.5 text-left text-xs text-fg-faint transition-colors hover:border-line hover:text-fg-muted"
      >
        <Search size={13} aria-hidden="true" />
        <span className="flex-1">搜索页面 / 事件</span>
        <Kbd>⌘K</Kbd>
      </button>

      <div className="mt-3 min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        {navGroups.map((group) => (
          <div key={group.label} className="mb-4">
            <p className="mb-1 px-2 text-[11px] font-medium text-fg-faint">{group.label}</p>
            <ul className="space-y-px">
              {group.items.map((item) => (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={item.to === "/"}
                    className={({ isActive }) => cx(
                      "flex h-8 items-center gap-2.5 rounded-md px-2 text-[13px] transition-colors",
                      isActive ? "bg-surface-2 font-medium text-fg" : "text-fg-muted hover:bg-surface-2/60 hover:text-fg",
                    )}
                  >
                    <item.icon size={15} aria-hidden="true" className="shrink-0" />
                    <span className="flex-1">{item.label}</span>
                    {item.to === "/incidents" && firing > 0 && (
                      <span title={`${firing} 个事件触发中`} className="tabular min-w-5 rounded-full bg-danger px-1.5 text-center text-[10.5px] font-semibold leading-[18px] text-white">
                        {firing}
                      </span>
                    )}
                  </NavLink>
                </li>
              ))}
            </ul>
          </div>
        ))}

        <div>
          <p className="mb-1 px-2 text-[11px] font-medium text-fg-faint">最近事件</p>
          {recent.length === 0 ? (
            <p className="px-2 py-1 text-xs text-fg-faint">暂无事件</p>
          ) : (
            <ul className="space-y-px">
              {recent.map((incident) => (
                <li key={incident.id}>
                  <NavLink
                    to={`/incidents/${incident.id}`}
                    title={incident.title}
                    className={({ isActive }) => cx(
                      "flex h-7 items-center gap-2 rounded-md px-2 text-[12.5px] transition-colors",
                      isActive ? "bg-surface-2 text-fg" : "text-fg-muted hover:bg-surface-2/60 hover:text-fg",
                    )}
                  >
                    <StatusDot tone={statusTone(incident.status)} className="size-1.5" />
                    <span className="min-w-0 flex-1 truncate">{incident.title || `Incident #${incident.id}`}</span>
                    <span className="tabular text-[11px] text-fg-faint">#{incident.id}</span>
                  </NavLink>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      <div className="flex shrink-0 items-center gap-1 border-t border-line-soft px-3 py-2">
        <ModelMenu />
        <button
          type="button"
          onClick={toggleTheme}
          aria-label={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
          title={theme === "dark" ? "浅色主题" : "深色主题"}
          className="rounded-md p-2 text-fg-faint hover:bg-surface-2 hover:text-fg"
        >
          {theme === "dark" ? <Sun size={14} aria-hidden="true" /> : <Moon size={14} aria-hidden="true" />}
        </button>
      </div>
    </nav>
  );
}
