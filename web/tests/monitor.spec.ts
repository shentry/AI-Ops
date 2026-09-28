import { readFileSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { controlRoom, openRoom } from "./fixtures";

// The real dashboard JSON the server embeds and Grafana provisions.
function dashboard(file: string) {
  return JSON.parse(readFileSync(new URL(`../../internal/grafana/dashboards/${file}`, import.meta.url), "utf8"));
}

const boards = [
  { uid: "sub2api", title: "sub2api 服务" },
  { uid: "monitoring-stack", title: "监控栈" },
];

function matrix(metric: Record<string, string>, values: [number, string][]) {
  return { metric, values };
}

interface RangeCall { expr: string; start: string; end: string; step: string }

async function openMonitor(page: Page, target = "/monitor") {
  const calls: RangeCall[] = [];
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/session") return route.fulfill({ json: { id: "ops", name: "值班", role: "viewer" } });
    if (path.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (path === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (path === "/api/v1/observability/dashboards") return route.fulfill({ json: { dashboards: boards } });
    if (path === "/api/v1/observability/dashboards/sub2api") return route.fulfill({ json: { dashboard: dashboard("1-sub2api.json") } });
    if (path === "/api/v1/observability/dashboards/monitoring-stack") return route.fulfill({ json: { dashboard: dashboard("4-monitoring-stack.json") } });
    if (path === "/api/v1/prometheus/query_range" && request.method() === "POST") {
      const body = request.postDataJSON() as RangeCall;
      calls.push(body);
      const now = Math.floor(Date.parse(body.end) / 1000);
      let result: unknown[] = [];
      if (body.expr === 'probe_success{job="sub2api-health"}') result = [matrix({ job: "sub2api-health" }, [[now - 60, "1"], [now, "1"]])];
      if (body.expr === "sub2api_probe_success") result = [matrix({}, [[now, "0"]])];
      if (body.expr === "sub2api_requests_5m") {
        result = [
          matrix({ class: "all" }, [[now - 120, "40"], [now - 60, "42"], [now, "45"]]),
          matrix({ class: "sla" }, [[now - 120, "30"], [now - 60, "31"], [now, "33"]]),
        ];
      }
      if (body.expr === "up") result = [matrix({ job: "loki" }, [[now, "1"]]), matrix({ job: "grafana" }, [[now, "0"]])];
      if (body.expr === "sub2api_request_duration_p95_seconds_5m") {
        return route.fulfill({ status: 400, json: { error: "prometheus bad_data: parse error" } });
      }
      return route.fulfill({ json: { result_type: "matrix", result, from: body.start, to: body.end } });
    }
    throw new Error(`Unexpected API request: ${request.method()} ${path}`);
  });
  await page.goto(target);
  await expect(page.getByRole("heading", { name: "监控", exact: true })).toBeVisible();
  return calls;
}

test("renders the provisioned dashboard natively, one query per target", async ({ page }) => {
  const calls = await openMonitor(page);
  await expect(page.getByRole("tab", { name: "sub2api 服务" })).toHaveAttribute("aria-selected", "true");

  // Value mappings: states read as text, not color alone.
  await expect(page.getByRole("region", { name: "健康探测", exact: true })).toContainText("UP");
  await expect(page.getByRole("region", { name: "业务探针", exact: true })).toContainText("DOWN");
  // legendFormat names the series; recharts draws one line each.
  const requests = page.getByRole("region", { name: "请求数 (最近 5m 窗口)", exact: true });
  await expect(requests.locator(".recharts-line")).toHaveCount(2);
  await expect(requests).toContainText("all");
  // A failing query stays inside its own panel.
  await expect(page.getByRole("region", { name: "请求 p95 耗时", exact: true })).toContainText("查询失败");
  await expect(page.getByRole("region", { name: "请求 p95 耗时", exact: true })).toContainText("parse error");
  // Logs panels are Grafana-only.
  await expect(page.getByRole("region", { name: "sub2api 日志", exact: true })).toHaveCount(0);

  // Every panel queries the same window with the 1h step.
  await expect.poll(() => calls.length).toBeGreaterThanOrEqual(10);
  const windows = new Set(calls.map((call) => `${call.start}|${call.end}|${call.step}`));
  expect(windows.size).toBe(1);
  expect(calls[0].step).toBe("15s");
  expect(Date.parse(calls[0].end) - Date.parse(calls[0].start)).toBe(3600_000);
});

test("board tabs switch dashboards; bar lists show each target's state", async ({ page }) => {
  await openMonitor(page);
  await page.getByRole("tab", { name: "监控栈" }).click();
  await expect(page).toHaveURL(/board=monitoring-stack/);
  const targets = page.getByRole("region", { name: "抓取目标", exact: true });
  await expect(targets.getByRole("row", { name: /loki/ })).toContainText("UP");
  await expect(targets.getByRole("row", { name: /grafana/ })).toContainText("DOWN");
});

test("a custom window from the URL is queried as-is and sizes the step", async ({ page }) => {
  const start = "2026-08-24T11:30:00.000Z";
  const end = "2026-08-24T15:30:00.000Z";
  const calls = await openMonitor(page, `/monitor?range=custom&start=${start}&end=${end}`);
  await expect.poll(() => calls.length).toBeGreaterThan(0);
  // 4 小时窗口按 6 小时预设取步长：6h / 360 = 60s。
  expect(calls.every((call) => call.start === start && call.end === end && call.step === "1m")).toBe(true);
  await expect(page.getByRole("button", { name: "时间范围" })).toContainText("08-24");
});

test("an incident links to Monitor on its own window", async ({ page }) => {
  const room = controlRoom();
  Object.assign(room.incident, { status: "resolved", resolved_at: "2026-08-24T12:20:00Z" });
  await openRoom(page, room);
  const href = await page.locator("header").getByRole("link", { name: "监控" }).getAttribute("href");
  const params = new URL(href!, "http://console").searchParams;
  expect(params.get("range")).toBe("custom");
  // 事件 12:00 开始、12:20 恢复，前后各留 30 分钟。
  expect(params.get("start")).toBe("2026-08-24T11:30:00.000Z");
  expect(params.get("end")).toBe("2026-08-24T12:50:00.000Z");
});
