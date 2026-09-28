import { expect, test } from "@playwright/test";
import { approval, controlRoom, openRoom } from "./fixtures";

const requiredFields = ["tool_name", "target", "target_id", "rule_id", "mode", "checks", "reason", "plan_hash", "expires_at"];
for (const field of requiredFields) {
  test(`missing ${field} visibly blocks approval`, async ({ page }) => {
    const incomplete: Record<string, unknown> = approval();
    delete incomplete[field];
    const state = await openRoom(page, controlRoom(incomplete));
    await expect(page.getByRole("alert")).toContainText("审批信息不完整");
    await expect(page.getByRole("button", { name: "批准执行" })).toBeDisabled();
    expect(state.decisions).toEqual([]);
  });
}

// Only a manual rule's snapshot waits for a person; anything else is never consent.
for (const mode of ["auto", "observe", "", 1]) {
  test(`mode ${JSON.stringify(mode)} cannot be approved by a person`, async ({ page }) => {
    await openRoom(page, controlRoom(approval({ mode })));
    await expect(page.getByRole("alert")).toContainText("人工审批模式");
    await expect(page.getByRole("button", { name: "批准执行" })).toBeDisabled();
  });
}

for (const [field, value] of [["expires_at", "not-a-date"], ["checks", []], ["target", "   "], ["target_id", " "]]) {
  test(`invalid ${field} visibly blocks approval`, async ({ page }) => {
    await openRoom(page, controlRoom(approval({ [field]: value })));
    await expect(page.getByRole("alert")).toContainText("审批信息不完整或无效");
    await expect(page.getByRole("button", { name: "批准执行" })).toBeDisabled();
  });
}

test("a complete manual snapshot shows every decision fact", async ({ page }) => {
  await openRoom(page, controlRoom(approval({ compensation: "upstream_restore" })));
  const panel = page.getByRole("region", { name: "等待你审批" });
  for (const text of ["container/sub2api", "c0ffee", "restart-stopped-process", "人工批准后执行", "容器实例、服务健康", "upstream_restore"]) {
    await expect(panel).toContainText(text);
  }
  await expect(panel.getByRole("button", { name: "批准执行" })).toBeEnabled();
});

for (const decision of ["approve", "deny"] as const) {
  test(`${decision} submits clicked hash and reason once despite refresh and double click`, async ({ page }) => {
    const original = approval();
    const state = await openRoom(page, controlRoom(original));
    let finish!: () => void;
    const gate = new Promise<void>((resolve) => { finish = resolve; });
    state.decide = async (route) => {
      await gate;
      state.room = controlRoom(approval({ status: decision === "approve" ? "approved" : "denied", verification: { status: decision === "approve" ? "not_started" : "not_applicable" } }));
      await route.fulfill({ json: { approval: state.room.latest_action } });
    };
    await page.getByLabel(/决策备注/).fill("  已核查目标  ");
    const button = page.getByRole("button", { name: decision === "approve" ? "批准执行" : "拒绝", exact: true });
    // Two synchronous DOM clicks exercise the in-flight guard before React can repaint.
    await button.evaluate((element: HTMLButtonElement) => { element.click(); element.click(); });
    await expect.poll(() => state.decisions.length).toBe(1);
    state.room = controlRoom(approval({ plan_hash: "b".repeat(64) }));
    await page.getByRole("button", { name: "刷新", exact: true }).click();
    await expect(button).toBeDisabled();
    expect(state.decisions).toEqual([{ path: `/api/v1/approvals/42/${decision}`, body: { plan_hash: original.plan_hash, reason: "已核查目标" } }]);
    finish();
    await expect(page.getByText(decision === "approve" ? "已批准，执行器会接管。" : "已拒绝。", { exact: true })).toBeVisible();
  });
}

test("an expired snapshot cannot be approved or denied", async ({ page }) => {
  const state = await openRoom(page, controlRoom(approval({ expires_at: "2000-01-01T00:00:00Z" })));
  await expect(page.getByRole("region", { name: "等待你审批" })).toContainText("已过期");
  await expect(page.getByRole("button", { name: "批准执行" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "拒绝", exact: true })).toBeDisabled();
  expect(state.decisions).toHaveLength(0);
});

test("hash conflict requires a new decision on the refreshed snapshot", async ({ page }) => {
  const state = await openRoom(page);
  const newHash = "b".repeat(64);
  state.decide = async (route) => {
    state.room = controlRoom(approval({ plan_hash: newHash, target_id: "d00d" }));
    await route.fulfill({ status: 409, json: { error: "plan_hash mismatch" } });
  };
  await page.getByRole("button", { name: "批准执行" }).click();
  await expect(page.getByText("plan_hash mismatch", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "等待你审批" })).toContainText(newHash);
  expect(state.decisions).toHaveLength(1);
  state.decide = async (route) => {
    state.room = controlRoom(approval({ status: "approved", plan_hash: newHash }));
    await route.fulfill({ json: { approval: state.room.latest_action } });
  };
  await page.getByRole("button", { name: "批准执行" }).click();
  await expect.poll(() => state.decisions.length).toBe(2);
  expect(state.decisions[1].body).toEqual({ plan_hash: newHash, reason: "" });
});

for (const finalStatus of ["expired", "approved"]) {
  test(`409 ${finalStatus} is visible and refreshes the stale decision`, async ({ page }) => {
    const state = await openRoom(page);
    state.decide = async (route) => {
      state.room = controlRoom(approval({ status: finalStatus, verification: { status: finalStatus === "expired" ? "not_applicable" : "not_started" } }));
      await route.fulfill({ status: 409, json: { error: "approval conflict: expired, decided or plan_hash mismatch" } });
    };
    await page.getByRole("button", { name: "批准执行" }).click();
    await expect(page.getByText(/approval conflict:/)).toBeVisible();
    await expect.poll(() => state.reads).toBeGreaterThan(1);
    await expect(page.getByRole("button", { name: "批准执行" })).toHaveCount(0);
    await expect(page.getByText("已批准，执行器会接管。", { exact: true })).toHaveCount(0);
    expect(state.decisions).toHaveLength(1);
  });
}
