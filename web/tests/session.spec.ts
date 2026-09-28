import { expect, test } from "@playwright/test";
import { controlRoom, openRoom } from "./fixtures";

test("signed-out console asks for a personal token and never stores it", async ({ page }) => {
  const logins: { body: unknown; header: string | undefined }[] = [];
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/session" && request.method() === "GET") return route.fulfill({ status: 401, json: { error: "unauthorized" } });
    if (path === "/api/v1/session" && request.method() === "POST") {
      logins.push({ body: request.postDataJSON(), header: request.headers()["x-requested-with"] });
      if (logins.length === 1) return route.fulfill({ status: 401, json: { error: "invalid token" } });
      return route.fulfill({ json: { id: "ops", name: "值班", role: "operator" } });
    }
    if (path.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    if (path === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: [] } });
    if (path.endsWith("/control-room")) return route.fulfill({ json: controlRoom() });
    if (path.endsWith("/stream")) return route.fulfill({ status: 204 });
    if (path.endsWith("/steps")) return route.fulfill({ json: { steps: [] } });
    if (path.endsWith("/conversation")) return route.fulfill({ json: { messages: [] } });
    if (path.endsWith("/reviews")) return route.fulfill({ json: { reviews: [] } });
    throw new Error(`Unexpected API request: ${request.method()} ${path}`);
  });
  await page.goto("/incidents/1");
  const token = page.getByLabel("个人令牌");
  await token.fill("wrong-token");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("alert")).toHaveText("令牌无效");
  await expect(token).toHaveValue("");

  await token.fill("o".repeat(64));
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page.getByRole("heading", { name: "Sub2API down", exact: true })).toBeVisible();
  await expect(page.getByRole("group", { name: "当前用户：值班（值班）" })).toBeVisible();
  expect(logins).toEqual([
    { body: { token: "wrong-token" }, header: "oncall-console" },
    { body: { token: "o".repeat(64) }, header: "oncall-console" },
  ]);
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + document.cookie)).not.toContain("o".repeat(64));
});

test("viewer sees the pending approval but cannot decide or ask", async ({ page }) => {
  const state = await openRoom(page, controlRoom(), "viewer");
  await expect(page.getByText("只读账号不能审批。")).toBeVisible();
  await expect(page.getByRole("button", { name: "批准执行" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "拒绝", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "重新诊断" })).toBeDisabled();
  expect(state.decisions).toEqual([]);
});
