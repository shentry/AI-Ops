import { expect, test } from "@playwright/test";
import { approval, controlRoom, openRoom } from "./fixtures";

const states = [
  ["pending", "not_started", "待处理", "尚未开始"],
  ["approved", "not_started", "已批准", "尚未开始"],
  ["executing", "not_started", "执行中", "尚未开始"],
  ["executed", "pending", "已执行", "等待恢复验证"],
  ["executed", "running", "已执行", "恢复验证中"],
  ["executed", "passed", "已执行", "恢复检查连续通过"],
  ["executed", "stable", "已执行", "恢复后观察期内未复发"],
  ["executed", "recurred", "已执行", "恢复后复发"],
  ["executed", "failed", "已执行", "观察窗口内未恢复"],
  ["executed", "inconclusive", "已执行", "恢复情况未知，需要人工核查"],
  ["aborted", "not_applicable", "执行前拒绝", "不适用"],
  ["failed", "not_applicable", "失败", "不适用"],
  ["denied", "not_applicable", "已拒绝", "不适用"],
  ["expired", "not_applicable", "已过期", "不适用"],
];

for (const [execution, verification, executionText, verificationText] of states) {
  test(`latest_action ${execution}/${verification} shows separate facts without pending_approval`, async ({ page }) => {
    const room = controlRoom(approval({ status: execution, verification: { status: verification } }));
    room.pending_approval = null;
    await openRoom(page, room);
    const action = page.getByRole("region", { name: "最近变更" });
    await expect(action).toContainText(executionText);
    await expect(action).toContainText(verificationText);
    await expect(action).toContainText("container/sub2api");
    await expect(page.getByText("没有待审批的变更", { exact: true })).toBeVisible();
    await expect(action).toContainText("restart-stopped-process");
    if (verification === "passed" || verification === "stable") await expect(action).toContainText("告警状态尚未同步");
    if (execution === "aborted") {
      await expect(action).toContainText("没有写入");
      await expect(action).not.toContainText("已执行");
    }
    if (execution === "failed") await expect(action).toContainText("人工核查");
  });
}

test("legacy history remains unknown and keeps execution and observation details", async ({ page }) => {
  await openRoom(page, controlRoom(approval({
    status: "executed", rule_id: "", mode: "", checks: [],
    decided_by: "anonymous", decision_reason: "人工核查旧记录",
    verification: { status: "unknown", last_checked_at: null, deadline_at: null },
  })));
  const action = page.getByRole("region", { name: "最近变更" });
  await expect(action).toContainText("旧版记录，执行上下文未知");
  await expect(action).toContainText("人工核查旧记录");
  await expect(action).not.toContainText("恢复检查连续通过");
  await expect(page.getByRole("button", { name: "批准执行" })).toHaveCount(0);
});

test("successful diagnosis alone is not recovery or execution", async ({ page }) => {
  await openRoom(page, controlRoom(null));
  await expect(page.getByRole("region", { name: "Incident 概览" })).toContainText("诊断完成");
  await expect(page.getByText("没有变更记录", { exact: true })).toBeVisible();
  await expect(page.getByText("已恢复", { exact: true })).toHaveCount(0);
  await expect(page.getByText("已执行", { exact: true })).toHaveCount(0);
});

for (const eventType of ["execution.aborted", "verify.queued", "verify.started", "verify.checked", "verify.passed", "verify.failed", "verify.inconclusive", "verify.stable", "verify.recurred"]) {
  test(`${eventType} named SSE event refreshes latest_action`, async ({ page }) => {
    const state = await openRoom(page, controlRoom(approval({ status: "executing" })));
    const aborted = eventType === "execution.aborted";
    const verification = eventType === "verify.started" ? "running" : eventType.split(".")[1];
    const nextVerification = ["passed", "failed", "inconclusive", "running", "stable", "recurred"].includes(verification) ? verification : "pending";
    state.room = controlRoom(approval({
      status: aborted ? "aborted" : "executed",
      verification: {
        status: aborted ? "not_applicable" : nextVerification,
        last_checked_at: "2026-08-24T12:01:00Z", deadline_at: "2026-08-24T12:03:00Z", detail: "等待下一次健康观测",
      },
    }));
    await state.emit(eventType);
    await expect.poll(() => state.reads).toBeGreaterThan(1);
    const action = page.getByRole("region", { name: "最近变更" });
    const verificationText = states.find(([execution, status]) => execution === "executed" && status === nextVerification)![3];
    await expect(action).toContainText(aborted ? "没有写入" : verificationText);
    if (!aborted) {
      await expect(action).toContainText("最近检查");
      await expect(action).toContainText("观察窗口截止");
      await expect(action).toContainText("等待下一次健康观测");
    }
  });
}

test("receipt, compensation and watch phase are shown as separate facts", async ({ page }) => {
  const room = controlRoom(approval({
    status: "executed", kind: "compensation", parent_approval_id: 41,
    result: { written: true, outcome: "written", before: "schedulable=true", after: "schedulable=false", detail: "account 7 quarantined" },
    verification: { status: "pending", phase: "watch" },
  }));
  room.pending_approval = null;
  await openRoom(page, room);
  const action = page.getByRole("region", { name: "最近变更" });
  for (const text of ["已写入", "schedulable=true → schedulable=false", "补偿审批 #41", "恢复后观察期"]) {
    await expect(action).toContainText(text);
  }
});

for (const [status, label] of [["not_started", "尚未开始"], ["not_applicable", "不适用"], ["unknown", "未知"]]) {
  test(`flow renders server verification status ${status} without guessing`, async ({ page }) => {
    const room = controlRoom(null);
    room.flow_nodes = [{ id: "verify", kind: "verify", name: "verify", status }];
    await openRoom(page, room);
    const flow = page.getByRole("region", { name: "处理流程" });
    await expect(flow).toContainText(label);
    await expect(flow).not.toContainText(status);
  });
}
