import { expect, test, type Page } from "@playwright/test";
import { approval, controlRoom, openRoom } from "./fixtures";

const remediation = {
  service: "sub2api", env: "prod", rules_version: "r1@abcdefabcdef", emergency_stop: false, maintenance: "",
  rules: [
    { id: "restart-stopped-process", action: "docker_restart", mode: "auto", alerts: ["Sub2APIDown"], max_executions: 2, window_minutes: 60, executions: 1, blocked: "" },
    { id: "quarantine-upstream", action: "upstream_quarantine", mode: "manual", alerts: ["Sub2APIUpstreamAccountErrors"], max_executions: 1, window_minutes: 60, executions: 1, blocked: "verify.failed on approval 9 at 2026-08-24T12:00:00Z" },
  ],
  events: [{ id: 1, kind: "rules_loaded", actor: "config", reason: "r1@abcdefabcdef", created_at: "2026-08-24T11:00:00Z" }],
};

const report = {
  since: "2026-07-25T00:00:00Z", until: "2026-08-24T00:00:00Z", incidents: 4,
  executions: { total: 2, reviewed: 0, wrong: 0, coverage: 0, error_rate: null },
  unattended: { recovered: 1, confirmed: 0, rate: 0.25, confirmed_rate: 0, in_scope: 2, in_scope_rate: 0.5 },
  recovery_minutes: { median: 3.5, max: 3.5 },
  recurrence: { watched: 0, recurred: 0, rate: null },
  manual: { incidents: 1, share: 0.25, minutes: 20 },
  cost: { tokens_in: 1000, tokens_out: 200, tokens_per_incident: 300 },
  root_cause: { reviewed: 0, correct: 0, partial: 0, wrong: 0, unknown: 0, accuracy: null },
  review_queue: [{ incident_id: 7, approval_id: 9, reason: "verification_failed" }],
};

const changes = [
  { id: 5, change_type: "release", release_id: "v0.1.165", image_ref: `weishaw/sub2api@sha256:${"a".repeat(64)}`, db_migration: "compatible", verified_at: null, occurred_at: "2026-08-24T10:00:00Z", source: "ci", actor: "machine" },
];

const headings: Record<string, string> = { "/remediation": "自动处置", "/report": "效果评估", "/changes": "发布记录" };

async function openConsolePage(page: Page, role = "admin", target = "/remediation") {
  const writes: { path: string; body: unknown }[] = [];
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/session") return route.fulfill({ json: { id: role, name: role, role } });
    if (path.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (path === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (request.method() === "POST") {
      writes.push({ path, body: request.postDataJSON() });
      return route.fulfill({ json: {} });
    }
    if (path === "/api/v1/remediation") return route.fulfill({ json: remediation });
    if (path === "/api/v1/remediation/report") return route.fulfill({ json: report });
    if (path === "/api/v1/changes") return route.fulfill({ json: { changes } });
    throw new Error(`Unexpected API request: ${request.method()} ${path}`);
  });
  await page.goto(target);
  await expect(page.getByRole("heading", { name: headings[target], exact: true })).toBeVisible();
  return writes;
}

test("rules and blocks are shown; a viewer has no controls", async ({ page }) => {
  await openConsolePage(page, "viewer", "/remediation");
  await expect(page.getByText("r1@abcdefabcdef").first()).toBeVisible();
  await expect(page.getByText("规则自动执行")).toBeVisible();
  await expect(page.getByText("已阻断")).toBeVisible();
  await expect(page.getByText("verify.failed on approval 9", { exact: false }).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "急停" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "复位" })).toHaveCount(0);
});

test("rates keep raw counts, an empty denominator is not a rate, and the review queue links incidents", async ({ page }) => {
  await openConsolePage(page, "viewer", "/report");
  // A zero denominator is not a zero error rate.
  await expect(page.getByRole("region", { name: "错误执行率" })).toContainText("样本不足");
  await expect(page.getByRole("region", { name: "整体无人介入恢复率" })).toContainText("25.0%");
  await expect(page.getByRole("region", { name: "支持范围内自动恢复率" })).toContainText("1 / 2");
  await expect(page.getByRole("link", { name: /Incident #7 · 动作 #9/ })).toHaveAttribute("href", "/incidents/7");
});

test("a viewer cannot mark a release healthy", async ({ page }) => {
  await openConsolePage(page, "viewer", "/changes");
  await expect(page.getByText("v0.1.165")).toBeVisible();
  await expect(page.getByRole("button", { name: "标记健康" })).toHaveCount(0);
});

test("admin stop and rule reset need a reason and are recorded", async ({ page }) => {
  const writes = await openConsolePage(page, "admin", "/remediation");
  await page.getByRole("button", { name: "急停" }).click();
  await expect(page.getByText("请先填写原因")).toBeVisible();
  expect(writes).toEqual([]);
  await page.getByLabel("原因（写入审计）").fill("bad deploy in progress");
  await page.getByRole("button", { name: "急停" }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0]).toEqual({ path: "/api/v1/remediation/stop", body: { reason: "bad deploy in progress" } });
  await expect(page.getByText(/已急停/)).toBeVisible();
  await page.getByLabel("原因（写入审计）").fill("upstream replaced");
  await page.getByRole("button", { name: "复位" }).click();
  await expect.poll(() => writes.length).toBe(2);
  expect(writes[1]).toEqual({ path: "/api/v1/remediation/rules/quarantine-upstream/reset", body: { reason: "upstream replaced" } });
});

test("an operator marks a release healthy but cannot stop automation", async ({ page }) => {
  const writes = await openConsolePage(page, "operator", "/changes");
  await page.getByRole("button", { name: "标记健康" }).click();
  await expect.poll(() => writes.length).toBe(1);
  expect(writes[0].path).toBe("/api/v1/changes/5/verify");
  await page.getByRole("link", { name: "处置规则" }).click();
  await expect(page.getByRole("heading", { name: "自动处置", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "急停" })).toHaveCount(0);
});

test("an operator reviews the action; the reviewer is never sent by the page", async ({ page }) => {
  const state = await openRoom(page, controlRoom(approval({ status: "executed", verification: { status: "failed" } })));
  const panel = page.getByRole("region", { name: "复盘标注" });
  await panel.getByLabel("结论").selectOption("wrong");
  await panel.getByLabel("真实根因").fill("数据库连接耗尽");
  await panel.getByLabel("实际处理").fill("重启 postgres");
  await panel.getByLabel("人工处理分钟数").fill("25");
  await panel.getByRole("button", { name: "保存复盘" }).click();
  await expect(panel).toContainText("该规则的自动执行已阻断");
  expect(state.reviews).toEqual([{ subject: "action", verdict: "wrong", approval_id: 42, root_cause: "数据库连接耗尽", actual_fix: "重启 postgres", manual_minutes: 25 }]);
});

test("a confirmed root cause can be added to the knowledge base", async ({ page }) => {
  const state = await openRoom(page, controlRoom(approval({ status: "executed", verification: { status: "passed" } })));
  const panel = page.getByRole("region", { name: "复盘标注" });
  // Nothing confirmed yet: the button says why instead of failing on click.
  await expect(panel.getByRole("button", { name: "加入知识库" })).toBeDisabled();
  await panel.getByLabel("结论").selectOption("unknown");
  await panel.getByLabel("真实根因").fill("还不确定");
  await panel.getByRole("button", { name: "保存复盘" }).click();
  await expect(panel.getByRole("button", { name: "加入知识库" })).toBeDisabled();
  await panel.getByLabel("结论").selectOption("partial");
  await panel.getByLabel("真实根因").fill("凭据轮换未同步");
  await panel.getByRole("button", { name: "保存复盘" }).click();
  await panel.getByRole("button", { name: "加入知识库" }).click();
  await expect(panel).toContainText("已加入知识库");
  expect(state.knowledgeAdds).toBe(1);
});

test("a viewer sees reviews but no review form", async ({ page }) => {
  await openRoom(page, controlRoom(approval({ status: "executed" })), "viewer");
  const panel = page.getByRole("region", { name: "复盘标注" });
  await expect(panel).toBeVisible();
  await expect(panel.getByRole("button", { name: "保存复盘" })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: "加入知识库" })).toHaveCount(0);
});
