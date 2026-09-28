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

async function openKnowledge(page: Page, response: { status?: number; json: unknown }) {
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/v1/session") return route.fulfill({ json: { id: "ops", name: "值班", role: "viewer" } });
    if (url.pathname.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (url.pathname === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (url.pathname === "/api/v1/skills" && route.request().method() === "GET") return route.fulfill(response);
    throw new Error(`Unexpected API request: ${route.request().method()} ${url.pathname}`);
  });
  await page.goto("/knowledge");
  await expect(page.getByRole("heading", { name: "知识库", exact: true })).toBeVisible();
}

test("the skills tab lists each skill with its alerts, tools, digest and activations", async ({ page }) => {
  await openKnowledge(page, { json: { skills } });
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
  await openKnowledge(page, { status: 503, json: { error: "skill activations are unavailable" } });
  await expect(page.getByRole("status")).toContainText("skill activations are unavailable");
});

test("the knowledge page is reachable from the navigation", async ({ page }) => {
  await openKnowledge(page, { json: { skills } });
  await expect(page.getByRole("link", { name: "知识库" }).first()).toHaveAttribute("href", "/knowledge");
});
