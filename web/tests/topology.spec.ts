import { expect, test, type Page } from "@playwright/test";
import { controlRoom, openRoom } from "./fixtures";

function topology(overrides: Record<string, unknown> = {}) {
  return {
    checked_at: "2026-08-24T12:00:05Z",
    nodes: [
      { id: "sub2api", kind: "service", container: "sub2api", state: "up", container_id: "c0ffee0123456789", restart_count: 2, alerts: ["Sub2APIDown"] },
      { id: "postgres", kind: "datastore", container: "sub2api-postgres", state: "down", detail: "health max(pg_up) = 0", container_id: "d00d", alerts: ["Sub2APIPostgresUnreachable"] },
      { id: "redis", kind: "datastore", container: "sub2api-redis", state: "missing", detail: "container sub2api-redis does not exist" },
      { id: "upstream", kind: "upstream", state: "unknown", detail: "no container or health check declared" },
      { id: "host", kind: "host", state: "up" },
    ],
    edges: [
      { from: "sub2api", to: "postgres", type: "depends_on" },
      { from: "sub2api", to: "redis", type: "depends_on" },
      { from: "sub2api", to: "upstream", type: "depends_on" },
      { from: "sub2api", to: "host", type: "runs_on" },
    ],
    highlight: [],
    ...overrides,
  };
}

async function openTopology(page: Page, body = topology(), target = "/topology") {
  const queries: string[] = [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/v1/session") return route.fulfill({ json: { id: "ops", name: "值班", role: "viewer" } });
    if (url.pathname.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (url.pathname === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (url.pathname === "/api/v1/topology") {
      queries.push(url.search);
      return route.fulfill({ json: body });
    }
    throw new Error(`Unexpected API request: ${route.request().method()} ${url.pathname}`);
  });
  await page.goto(target);
  await expect(page.getByRole("heading", { name: "拓扑", exact: true })).toBeVisible();
  return queries;
}

test("the topology shows each node's live state and opens its facts", async ({ page }) => {
  await openTopology(page);
  // Every node is drawn; state reads as text, not color alone.
  await expect(page.getByTestId("topology-node-postgres")).toContainText("故障");
  await expect(page.getByTestId("topology-node-redis")).toContainText("不存在");
  await expect(page.getByTestId("topology-node-upstream")).toContainText("未知");
  await expect(page.getByTestId("topology-node-sub2api").getByLabel("1 条告警")).toBeVisible();
  await expect(page.getByText("5 节点 · 4 关系")).toBeVisible();
  // Every relation is drawn: React Flow skips an edge whose nodes lost their measured handles.
  await expect(page.locator(".react-flow__edge")).toHaveCount(4);

  const nodes = page.getByRole("list", { name: "节点" });
  await expect(nodes.getByRole("button", { name: /postgres/ })).toContainText("1 告警");
  await nodes.getByRole("button", { name: /postgres/ }).click();
  const drawer = page.getByRole("complementary", { name: "节点 postgres" });
  await expect(drawer).toContainText("health max(pg_up) = 0");
  await expect(drawer).toContainText("Sub2APIPostgresUnreachable");
  await expect(drawer).toContainText("sub2api 依赖本节点");
  await expect(drawer.getByRole("link", { name: "查看监控" })).toHaveAttribute("href", "/monitor?board=dependencies-host");

  // Clicking a node on the canvas selects it too.
  await page.getByTestId("topology-node-sub2api").click();
  const service = page.getByRole("complementary", { name: "节点 sub2api" });
  await expect(service).toContainText("c0ffee012345");
  await expect(service).toContainText("依赖 postgres");
  await expect(service).toContainText("运行于 host");
  await expect(service.getByRole("link", { name: "查看监控" })).toHaveAttribute("href", "/monitor?board=sub2api");
  await service.getByRole("button", { name: "关闭" }).click();
  await expect(page.getByRole("list", { name: "节点" })).toBeVisible();
});

test("an unreadable alert source is said, not shown as no alerts", async ({ page }) => {
  await openTopology(page, topology({ alerts_error: "firing alerts could not be read" }));
  await expect(page.getByText("告警读取失败")).toBeVisible();
});

test("an incident links to the topology for its own alerts", async ({ page }) => {
  const room = controlRoom();
  await openRoom(page, room);
  await expect(page.locator("header").getByRole("link", { name: "拓扑" })).toHaveAttribute("href", `/topology?incident=${room.incident.id}`);
});

test("?incident asks the server which nodes to highlight", async ({ page }) => {
  const queries = await openTopology(page, topology({ highlight: ["postgres"] }), "/topology?incident=7");
  expect(queries[0]).toBe("?incident=7");
  await expect(page.getByText("已高亮事件 #7 的告警所涉及的节点。")).toBeVisible();
  await expect(page.getByRole("list", { name: "节点" }).getByRole("button", { name: /postgres/ })).toContainText("事件");
  await expect(page.getByRole("link", { name: "事件 #7" })).toHaveAttribute("href", "/incidents/7");
});
