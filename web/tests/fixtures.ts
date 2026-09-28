import { expect, type Page, type Route } from "@playwright/test";

export function approval(overrides: Record<string, unknown> = {}) {
  return {
    id: 42, incident_id: 1, run_id: 7, status: "pending",
    tool_name: "docker_restart", kind: "primary", target: "container/sub2api", target_id: "c0ffee",
    rule_id: "restart-stopped-process", rule_version: "r1@abcdefabcdef", mode: "manual", revision: "started_at=2026-08-24T12:00:00Z",
    checks: ["container", "health"], compensation: "", reason: "规则要求人工批准",
    plan_hash: "a".repeat(64), expires_at: "2099-08-24T12:30:00Z",
    verification: { status: "not_started" }, ...overrides,
  };
}

export function controlRoom(action: Record<string, unknown> | null = approval()) {
  return {
    incident: {
      id: 1, group_key: "sub2api", status: "firing", severity: 1, alerts_count: 1,
      title: "Sub2API down", started_at: "2026-08-24T12:00:00Z", last_seen_at: "2026-08-24T12:00:00Z",
    },
    members: [], current_run: { id: 7, incident_id: 1, status: "succeeded", mode: "full", started_at: "2026-08-24T12:00:00Z" },
    flow_nodes: [] as { id: string; kind: string; name: string; status: string }[], open_problems: [], recent_events: [],
    pending_approval: action?.status === "pending" ? action : null,
    latest_action: action,
  };
}

// Every API request is intercepted, including unexpected writes. No backend is used.
export async function openRoom(page: Page, initial = controlRoom(), role = "operator") {
  let stream: Route | undefined;
  const state = {
    room: initial,
    reads: 0,
    decisions: [] as { path: string; body: Record<string, unknown> }[],
    decide: async (route: Route) => route.fulfill({ json: { approval: approval({ status: "approved" }) } }),
    reviews: [] as Record<string, unknown>[],
    steps: [] as Record<string, unknown>[],
    feed: [] as Record<string, unknown>[],
    knowledgeAdds: 0,
  };
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/session")) return route.fulfill({ json: { id: "ops", name: "值班", role } });
    // The console shell polls the incident feed for the sidebar and the command palette.
    if (path === "/api/v1/control-room/incidents") return route.fulfill({ json: { incidents: state.feed } });
    if (path.endsWith("/control-room")) {
      state.reads += 1;
      return route.fulfill({ json: state.room });
    }
    if (path.endsWith("/stream")) {
      if (!stream) { stream = route; return; }
      return route.fulfill({ status: 204 });
    }
    if (/\/approvals\/\d+\/(approve|deny)$/.test(path)) {
      state.decisions.push({ path, body: route.request().postDataJSON() });
      return state.decide(route);
    }
    if (path.endsWith("/reviews")) {
      if (route.request().method() === "POST") {
        const body = route.request().postDataJSON() as Record<string, unknown>;
        state.reviews.push(body);
        return route.fulfill({ status: 201, json: { id: state.reviews.length, incident_id: 1, reviewer: "ops", created_at: "2026-08-24T12:10:00Z", ...body } });
      }
      return route.fulfill({ json: { reviews: [] } });
    }
    if (path.endsWith("/knowledge") && route.request().method() === "POST") {
      state.knowledgeAdds += 1;
      return route.fulfill({ status: 201, json: { id: 9, source: "incident", ref: "incident/1", title: "Incident #1 复盘", created_by: "ops", updated_at: "2026-08-24T12:11:00Z" } });
    }
    if (path.endsWith("/steps")) return route.fulfill({ json: { steps: state.steps } });
    if (path.endsWith("/conversation")) return route.fulfill({ json: { messages: [] } });
    if (path.endsWith("/model")) return route.fulfill({ json: { current_model: "test", models: [] } });
    throw new Error(`Unexpected API request: ${route.request().method()} ${path}`);
  });
  await page.goto("/incidents/1");
  await expect(page.getByRole("heading", { name: "Sub2API down", exact: true })).toBeVisible();
  return Object.assign(state, {
    async emit(eventType: string) {
      await expect.poll(() => Boolean(stream)).toBe(true);
      const event = { id: 100, incident_id: 1, approval_id: 42, event_type: eventType, phase: "execution", status: "recorded", summary: "测试事件", created_at: "2026-08-24T12:01:00Z" };
      await stream!.fulfill({ contentType: "text/event-stream", body: `retry: 3600000\nid: 100\nevent: ${eventType}\ndata: ${JSON.stringify(event)}\n\n` });
    },
  });
}
