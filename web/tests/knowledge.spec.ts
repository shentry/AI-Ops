import { expect, test, type Page } from "@playwright/test";

const skills = [
  {
    name: "container_exit_oom", description: "sub2api 健康探测失败、容器反复重启或出现 OOM",
    alerts: ["Sub2APIDown", "Sub2APIContainerOOM"], tools: ["docker_inspect", "docker_logs"],
    sha256: "ab12cd34ef56".padEnd(64, "0"), body: "## 排查步骤\n1. 先读 docker_inspect 证据的 facts", activations_30d: 3,
  },
  {
    name: "redis_unreachable", description: "Redis 不可达", alerts: ["Sub2APIRedisUnreachable"], tools: ["prom_instant_query"],
    sha256: "f".repeat(64), body: "## 排查步骤\n1. 看拓扑证据", activations_30d: 0,
  },
];

const entries = [
  { id: 1, source: "repo", ref: "dependencies.md#PostgreSQL 常见错误的含义", title: "PostgreSQL 与 Redis 依赖 / PostgreSQL 常见错误的含义", created_by: "repo", updated_at: "2026-09-28T02:00:00Z" },
  { id: 9, source: "incident", ref: "incident/3", title: "Incident #3 复盘：Sub2API business errors", created_by: "ops", updated_at: "2026-09-28T03:00:00Z" },
];

async function openKnowledge(page: Page, options: { role?: string; skillsStatus?: number; target?: string } = {}) {
  const state = { searches: [] as string[], deleted: [] as string[] };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    if (url.pathname === "/api/v1/session") return route.fulfill({ json: { id: "ops", name: "值班", role: options.role ?? "viewer" } });
    if (url.pathname.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (url.pathname === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (url.pathname === "/api/v1/skills" && method === "GET") {
      return options.skillsStatus ? route.fulfill({ status: options.skillsStatus, json: { error: "skill activations are unavailable" } }) : route.fulfill({ json: { skills } });
    }
    if (url.pathname === "/api/v1/knowledge" && method === "GET") {
      state.searches.push(url.search);
      const q = url.searchParams.get("q");
      return route.fulfill({ json: { entries: q ? [{ ...entries[0], snippet: "SQLSTATE 28P01：应用账号凭据错误" }] : entries } });
    }
    if (url.pathname === "/api/v1/knowledge/9" && method === "GET") return route.fulfill({ json: { ...entries[1], body: "## 根因\n凭据轮换未同步" } });
    if (url.pathname === "/api/v1/knowledge/1" && method === "GET") return route.fulfill({ json: { ...entries[0], body: "## PostgreSQL 常见错误的含义\n28P01" } });
    if (url.pathname === "/api/v1/knowledge/9" && method === "DELETE") {
      state.deleted.push(url.pathname);
      return route.fulfill({ status: 204 });
    }
    throw new Error(`Unexpected API request: ${method} ${url.pathname}`);
  });
  await page.goto(options.target ?? "/knowledge");
  await expect(page.getByRole("heading", { name: "知识库", exact: true })).toBeVisible();
  return state;
}

test("the skills tab lists each skill with its alerts, tools, digest and activations", async ({ page }) => {
  await openKnowledge(page);
  await expect(page.getByRole("tab", { name: /排查技能/ })).toHaveAttribute("aria-selected", "true");
  const skill = page.getByRole("article", { name: "技能 container_exit_oom" });
  await expect(skill).toContainText("Sub2APIDown");
  await expect(skill).toContainText("docker_logs");
  await expect(skill).toContainText("近 30 天激活 3 次");
  await expect(skill).toContainText("ab12cd34ef56");
  // The body stays folded until asked for.
  await expect(skill.getByText("先读 docker_inspect 证据的 facts")).toBeHidden();
  await skill.getByText("查看排查步骤").click();
  await expect(skill.getByText("先读 docker_inspect 证据的 facts")).toBeVisible();
  await expect(page.getByRole("article", { name: "技能 redis_unreachable" })).toContainText("近 30 天激活 0 次");
});

test("an unavailable skills list says so instead of showing an empty catalog", async ({ page }) => {
  await openKnowledge(page, { skillsStatus: 503 });
  await expect(page.getByRole("status")).toContainText("skill activations are unavailable");
});

test("the reference tab searches, filters and reads entries", async ({ page }) => {
  const state = await openKnowledge(page);
  await page.getByRole("tab", { name: /参考文档/ }).click();
  await expect(page).toHaveURL(/tab=docs/);
  await expect(page.getByRole("button", { name: /Incident #3 复盘/ })).toContainText("事件复盘");
  await page.getByLabel("搜索参考文档").fill("数据库密码错误");
  await page.getByLabel("来源").selectOption("repo");
  await page.getByRole("button", { name: "搜索", exact: true }).click();
  await expect(page.getByText("SQLSTATE 28P01：应用账号凭据错误")).toBeVisible();
  expect(state.searches.at(-1)).toBe(`?q=${encodeURIComponent("数据库密码错误")}&source=repo`);
  await page.getByRole("button", { name: /PostgreSQL 常见错误的含义/ }).click();
  const entry = page.getByRole("article", { name: /PostgreSQL 常见错误的含义/ });
  await expect(entry).toContainText("仓库手册");
  // Repository entries change only through the repository, and a viewer deletes nothing.
  await expect(entry.getByRole("button", { name: "删除" })).toHaveCount(0);
  await entry.getByRole("button", { name: "返回列表" }).click();
  await expect(page.getByRole("search")).toBeVisible();
});

test("an admin removes an incident entry after confirming", async ({ page }) => {
  const state = await openKnowledge(page, { role: "admin", target: "/knowledge?tab=docs" });
  await page.getByRole("button", { name: /Incident #3 复盘/ }).click();
  const entry = page.getByRole("article", { name: /Incident #3 复盘/ });
  await expect(entry.getByRole("link", { name: "打开 Incident" })).toHaveAttribute("href", "/incidents/3");
  await entry.getByRole("button", { name: "删除" }).click();
  expect(state.deleted).toHaveLength(0);
  await entry.getByRole("button", { name: "确认删除" }).click();
  await expect(page.getByRole("status")).toContainText("已删除");
  expect(state.deleted).toEqual(["/api/v1/knowledge/9"]);
});

test("the knowledge page is reachable from the navigation", async ({ page }) => {
  await openKnowledge(page);
  await expect(page.getByRole("link", { name: "知识库" }).first()).toHaveAttribute("href", "/knowledge");
});
