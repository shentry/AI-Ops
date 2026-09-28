import { expect, test, type Page, type Route } from "@playwright/test";
import { approval, controlRoom, openRoom } from "./fixtures";

const incidents = [
  { id: 1, group_key: "sub2api", status: "firing", severity: 1, alerts_count: 3, title: "Sub2API down", started_at: "2026-08-24T12:00:00Z", last_seen_at: "2026-08-24T12:05:00Z" },
  { id: 2, group_key: "postgres", status: "resolved", severity: 2, alerts_count: 1, title: "PostgreSQL connections exhausted", started_at: "2026-08-23T08:00:00Z", last_seen_at: "2026-08-23T08:20:00Z", resolved_at: "2026-08-23T08:30:00Z" },
];

// A handler returns the fulfill promise when it owns the request, or undefined to fall through.
type Handler = (route: Route, path: string, url: URL) => Promise<void> | undefined;

// Every API request is intercepted; unknown ones fail the test.
async function openConsole(page: Page, target: string, handle: Handler = () => undefined) {
  const seen: string[] = [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    seen.push(`${path}${url.search}`);
    if (path === "/api/v1/session") return route.fulfill({ json: { id: "ops", name: "值班", role: "operator" } });
    if (path.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    const handled = handle(route, path, url);
    if (handled) return handled;
    if (path === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: url.searchParams.get("status") ? incidents.filter((row) => row.status === url.searchParams.get("status")) : incidents } });
    throw new Error(`Unexpected API request: ${route.request().method()} ${path}`);
  });
  await page.goto(target);
  return seen;
}

test("RCA markdown keeps formatting but never renders raw HTML, images or script links", async ({ page }) => {
  const room = controlRoom(null);
  room.current_run = {
    ...room.current_run,
    rca_text: "**连接池耗尽**导致 5xx。\n\n<img src=x onerror=\"window.__pwned=1\">\n\n<script>window.__pwned=2</script>\n\n![图](https://example.com/x.png)\n\n[点我](javascript:window.__pwned=3)",
  } as typeof room.current_run;
  await openRoom(page, room);
  const report = page.getByRole("region", { name: "诊断报告" });
  await expect(report.locator("strong", { hasText: "连接池耗尽" })).toBeVisible();
  await expect(report.locator("img")).toHaveCount(0);
  await expect(report.locator("script")).toHaveCount(0);
  const link = report.getByRole("link", { name: "点我" });
  if (await link.count()) expect(await link.getAttribute("href") ?? "").not.toMatch(/^javascript:/i);
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
});

test("a flow node opens the diagnosis trace with that step expanded and secrets redacted", async ({ page }) => {
  const room = controlRoom(null);
  room.flow_nodes = [{ id: "evidence", kind: "evidence", name: "evidence", status: "succeeded", step_id: 11 } as never];
  const state = await openRoom(page, room);
  state.steps = [
    { id: 10, run_id: 7, seq: 1, kind: "collector", name: "docker_ps", status: "succeeded", started_at: "2026-08-24T12:00:00Z", output: { containers: 3 } },
    { id: 11, run_id: 7, seq: 2, kind: "collector", name: "postgres_stats", status: "succeeded", started_at: "2026-08-24T12:00:01Z", input: { dsn: "postgres://u:hunter2@db/x", query: "select 1" }, output: { active: 97 } },
  ];
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await page.getByRole("region", { name: "处理流程" }).getByRole("button", { name: /证据采集/ }).click();
  await expect(page.getByRole("tab", { name: /诊断轨迹/ })).toHaveAttribute("aria-selected", "true");
  const step = page.getByRole("button", { name: /postgres_stats/ });
  await expect(step).toHaveAttribute("aria-expanded", "true");
  const panel = page.getByRole("tabpanel");
  await expect(panel).toContainText("select 1");
  await expect(panel).toContainText("[已脱敏]");
  await expect(panel).not.toContainText("hunter2");
});

test("overview shows firing incidents and the pending decisions that link to their incident", async ({ page }) => {
  await openConsole(page, "/", (route, path) => {
    if (path === "/api/v1/approvals") return route.fulfill({ json: { approvals: [approval({ incident_id: 1 })] } });
    if (path === "/api/v1/remediation") return route.fulfill({ json: { service: "sub2api", env: "prod", rules_version: "r1", emergency_stop: false, maintenance: "", rules: [], events: [] } });
    if (path === "/api/v1/remediation/report") return route.fulfill({ status: 503, json: { error: "report unavailable" } });
    return undefined;
  });
  await expect(page.getByRole("heading", { name: "概览", exact: true })).toBeVisible();
  const decisions = page.getByRole("region", { name: "等你决策" });
  await expect(decisions).toContainText("docker_restart");
  await expect(decisions.getByRole("link")).toHaveAttribute("href", "/incidents/1");
  await expect(page.getByRole("region", { name: "正在发生" })).toContainText("Sub2API down");
  await expect(page.getByRole("region", { name: "正在发生" })).not.toContainText("PostgreSQL");
  // One failed section does not hide the others.
  await expect(page.getByRole("region", { name: "值班概况" })).toContainText("report unavailable");
  await expect(page.getByRole("navigation", { name: "主导航" }).getByRole("link", { name: /事件/ }).first()).toContainText("1");
});

test("the incident list filters by server status and locally by text", async ({ page }) => {
  const seen = await openConsole(page, "/incidents?status=firing");
  await expect(page.getByRole("main").getByRole("link", { name: "Sub2API down", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "已恢复" }).click();
  await expect(page.getByRole("main").getByRole("link", { name: "PostgreSQL connections exhausted", exact: true })).toBeVisible();
  await expect(page.getByRole("main").getByRole("link", { name: "Sub2API down", exact: true })).toHaveCount(0);
  expect(seen).toContain("/api/v1/control-room/incidents?limit=100&status=resolved");
  await page.getByRole("button", { name: "全部" }).click();
  await page.getByLabel("过滤事件").fill("postgres");
  await expect(page.getByRole("table").getByRole("link")).toHaveCount(1);
});

test("the command palette jumps to an incident from the keyboard", async ({ page }) => {
  await openConsole(page, "/incidents", (route, path) => {
    if (path.endsWith("/control-room") || path.endsWith("/stream")) return route.fulfill({ status: 404, json: { error: "not in this test" } });
    return undefined;
  });
  await expect(page.getByRole("main").getByRole("link", { name: "Sub2API down", exact: true })).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  const dialog = page.getByRole("dialog", { name: "快速跳转" });
  await expect(dialog).toBeVisible();
  await dialog.getByLabel("搜索页面或事件").fill("postgres");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/incidents\/2$/);
  await expect(dialog).toHaveCount(0);
});
