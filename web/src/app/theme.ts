// 主题偏好只是本机便利设置：读写失败（隐私模式、禁用存储）时回到深色默认。
export type Theme = "dark" | "light";

const storageKey = "oncall-theme";
const themeColor: Record<Theme, string> = { dark: "#16181e", light: "#f5f6f9" };

export function readTheme(): Theme {
  try {
    return window.localStorage.getItem(storageKey) === "light" ? "light" : "dark";
  } catch {
    return "dark";
  }
}

export function applyTheme(theme: Theme, persist = false): void {
  document.documentElement.classList.toggle("dark", theme === "dark");
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", themeColor[theme]);
  if (!persist) return;
  try {
    window.localStorage.setItem(storageKey, theme);
  } catch {
    // The choice still applies for this page view.
  }
}
