import { expect, test } from "@playwright/test";
import { decideApproval, getControlRoom } from "../src/api";
import { approval, controlRoom } from "./fixtures";

const originalFetch = globalThis.fetch;
test.afterEach(() => { globalThis.fetch = originalFetch; });

test("approval contract preserves rule snapshot, receipt and verification metadata", async () => {
  const dto = approval({
    result: { written: true, outcome: "written", before: "a", after: "b", manual_check: false },
    verification: { status: "pending", phase: "watch", last_checked_at: null, deadline_at: "2026-08-24T12:03:00Z", detail: "等待观测" },
  });
  globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(dto)));
  const room = await getControlRoom(1);
  const result = room.pending_approval!;
  expect([result.rule_id, result.mode, result.target_id, result.checks]).toEqual(["restart-stopped-process", "manual", "c0ffee", ["container", "health"]]);
  expect(result.result).toMatchObject({ written: true, outcome: "written", before: "a", after: "b" });
  expect(result).not.toHaveProperty("risk");
  expect(room.latest_action).toEqual(room.pending_approval);
  expect(room.latest_action?.verification).toEqual(dto.verification);
});

test("absent action stays null; missing snapshot fields are empty, never inferred", async () => {
  globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(null)));
  expect((await getControlRoom(1)).latest_action).toBeNull();
  const dto: Record<string, unknown> = approval({ risk: "low", dry_run: false });
  for (const key of ["mode", "rule_id", "checks", "verification"]) delete dto[key];
  globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(dto)));
  const result = (await getControlRoom(1)).pending_approval;
  expect([result?.mode, result?.rule_id, result?.checks, result?.result]).toEqual(["", "", [], null]);
  expect(result?.verification.status).toBe("unknown");
  expect(result).not.toHaveProperty("dry_run");
});

for (const approve of [true, false]) {
  test(`decision contract always carries snapshot hash and reason (${approve})`, async () => {
    let request: RequestInit | undefined;
    globalThis.fetch = async (_path, init) => { request = init; return new Response(JSON.stringify(approval())); };
    await decideApproval(42, approve, "clicked-snapshot-hash", "");
    expect(request?.method).toBe("POST");
    expect(JSON.parse(String(request?.body))).toEqual({ plan_hash: "clicked-snapshot-hash", reason: "" });
  });
}
