import { expect, test } from "@playwright/test";
import { decideApproval, getControlRoom } from "../src/api";
import { approval, controlRoom } from "./fixtures";

const originalFetch = globalThis.fetch;
test.afterEach(() => { globalThis.fetch = originalFetch; });

for (const dryRun of [false, true, null]) {
  test(`approval contract preserves dry_run=${dryRun} and verification metadata`, async () => {
    const dto = approval({ dry_run: dryRun, verification: { status: "pending", last_checked_at: null, deadline_at: "2026-08-24T12:03:00Z", detail: "等待观测" } });
    globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(dto)));
    const room = await getControlRoom(1);
    expect(room.pending_approval?.dry_run).toBe(dryRun);
    expect(room.pending_approval?.safety_level).toBe("L2");
    expect(room.pending_approval).not.toHaveProperty("risk");
    expect(room.latest_action).toEqual(room.pending_approval);
    expect(room.latest_action?.verification).toEqual(dto.verification);
  });
}

test("absent action stays null; risk cannot replace safety_level", async () => {
  globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(null)));
  expect((await getControlRoom(1)).latest_action).toBeNull();
  const dto: Record<string, unknown> = approval({ risk: "L3" });
  delete dto.safety_level;
  delete dto.dry_run;
  delete dto.verification;
  globalThis.fetch = async () => new Response(JSON.stringify(controlRoom(dto)));
  const result = (await getControlRoom(1)).pending_approval;
  expect(result?.safety_level).toBe("");
  expect(result?.dry_run).toBeNull();
  expect(result?.verification.status).toBe("unknown");
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
