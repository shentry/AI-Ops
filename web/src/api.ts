export type JsonObject = Record<string, unknown>;

// Session is the identity the server proved for this browser. Roles nest:
// viewer reads, operator also decides/asks/rediagnoses, admin also switches models.
export type Role = "viewer" | "operator" | "admin";

export interface Session {
  id: string;
  name: string;
  role: Role;
}

export function canOperate(session: Session): boolean {
  return session.role === "operator" || session.role === "admin";
}

// Dispatched whenever the server rejects the session so the app can sign out.
export const unauthorizedEvent = "oncall:unauthorized";

export interface Incident {
  id: number;
  group_key: string;
  status: string;
  severity: number | string;
  alerts_count: number;
  title: string;
  started_at: string;
  last_seen_at: string;
  resolved_at?: string | null;
}

export interface IncidentMember {
  fingerprint: string;
  name: string;
  status: string;
  severity: number;
  linked_at: string;
}

export interface Run {
  id: number;
  incident_id: number;
  mode: string;
  status: string;
  retry_of?: number | null;
  started_at: string;
  finished_at?: string | null;
  rca_text?: string | null;
  tokens_in?: number;
  tokens_out?: number;
}

export interface RunStep {
  id: number;
  run_id: number;
  seq: number;
  kind: string;
  name: string;
  status?: string;
  input?: unknown;
  output?: unknown;
  error?: string | null;
  started_at: string;
  finished_at?: string | null;
  duration_ms?: number | null;
  owner?: string | null;
  truncated?: boolean;
}

export interface FlowNode {
  id: string;
  key?: string;
  name: string;
  phase?: string;
  status: string;
  started_at?: string | null;
  finished_at?: string | null;
  duration_ms?: number | null;
  timeout_ms?: number | null;
  error?: string | null;
  owner?: string | null;
  retries?: number;
  step_id?: number | null;
  run_id?: number | null;
}

export interface EventDTO {
  id: number;
  incident_id: number;
  run_id?: number | null;
  approval_id?: number | null;
  event_type: string;
  phase: string;
  status: string;
  summary: string;
  payload?: JsonObject | null;
  created_at: string;
}

export interface ProblemDTO {
  id: number;
  incident_id: number;
  run_id?: number | null;
  code: string;
  severity: string;
  status: string;
  summary: string;
  detail?: unknown;
  first_seen_at?: string;
  last_seen_at?: string;
  resolved_at?: string | null;
}

// ApprovalDTO is the frozen snapshot a person approves. Snapshot fields are
// empty when the snapshot does not validate against its plan hash.
export interface ApprovalDTO {
  id: number;
  incident_id: number;
  run_id?: number | null;
  parent_approval_id?: number | null;
  tool_name: string;
  plan_hash: string;
  reason: string;
  kind: string;
  target: string;
  target_id: string;
  rule_id: string;
  rule_version: string;
  mode: string;
  revision: string;
  checks: string[];
  compensation: string;
  result: Receipt | null;
  verification: {
    status: string;
    phase?: string;
    last_checked_at?: string | null;
    deadline_at?: string | null;
    detail?: string;
  };
  status: string;
  expires_at: string;
  created_at?: string;
  decided_by?: string | null;
  decided_at?: string | null;
  decision_reason?: string | null;
  decision_source?: string | null;
}

export interface ConversationCitation {
  id?: number;
  kind?: string;
  type?: string;
  label?: string;
  event_id?: number;
  step_id?: number;
  quote?: string;
  reference?: string;
}

export interface SuggestedAction {
  type?: string;
  target?: string;
  reason?: string;
  description?: string;
}

export interface ConversationMessage {
  id: number;
  incident_id: number;
  run_id?: number | null;
  reply_to_id?: number | null;
  channel: string;
  role: string;
  actor_id?: string | null;
  actor_name?: string | null;
  content: string;
  tool_name?: string | null;
  tool_call_id?: string | null;
  status: string;
  citations?: ConversationCitation[];
  uncertainties?: string[];
  suggested_actions?: SuggestedAction[];
  needs_user_input?: boolean;
  created_at: string;
  finished_at?: string | null;
}

export interface ControlRoom {
  incident: Incident;
  members: IncidentMember[];
  current_run: Run | null;
  flow_nodes: FlowNode[];
  open_problems: ProblemDTO[];
  pending_approval: ApprovalDTO | null;
  latest_action: ApprovalDTO | null;
  recent_events: EventDTO[];
}

export interface ModelOption {
  id: string;
  thinking_enabled: boolean;
}

export interface ModelState {
  current_model: string;
  updated_at: string;
  models: ModelOption[];
}

export class ApiError extends Error {
  readonly status: number;
  readonly body: unknown;

  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

function object(value: unknown): JsonObject {
  return value !== null && typeof value === "object" ? (value as JsonObject) : {};
}

function normalizedKey(value: string): string {
  return value.replace(/[_-]/g, "").toLowerCase();
}

function read(value: unknown, ...keys: string[]): unknown {
  const source = object(value);
  for (const key of keys) {
    if (key in source) return source[key];
    const match = Object.keys(source).find((candidate) => normalizedKey(candidate) === normalizedKey(key));
    if (match) return source[match];
  }
  return undefined;
}

function text(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : value === null || value === undefined ? fallback : String(value);
}

function optionalText(value: unknown): string | null | undefined {
  if (value === null) return null;
  if (value === undefined) return undefined;
  return text(value);
}

function number(value: unknown, fallback = 0): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return fallback;
}

function boolean(value: unknown, fallback = false): boolean {
  if (typeof value === "boolean") return value;
  if (typeof value === "string") return value.toLowerCase() === "true";
  return fallback;
}

function list(value: unknown, key: string): unknown[] {
  const candidate = Array.isArray(value) ? value : read(value, key);
  return Array.isArray(candidate) ? candidate : [];
}

function parseJSON(value: unknown): unknown {
  if (typeof value !== "string") return value;
  try {
    return JSON.parse(value) as unknown;
  } catch {
    return value;
  }
}

function toIncident(value: unknown): Incident {
  return {
    id: number(read(value, "id")),
    group_key: text(read(value, "group_key", "groupKey")),
    status: text(read(value, "status")),
    severity: numberOrText(read(value, "severity")),
    alerts_count: number(read(value, "alerts_count", "alertsCount")),
    title: text(read(value, "title")),
    started_at: text(read(value, "started_at", "startedAt")),
    last_seen_at: text(read(value, "last_seen_at", "lastSeenAt")),
    resolved_at: optionalText(read(value, "resolved_at", "resolvedAt")),
  };
}

function toMember(value: unknown): IncidentMember {
  return {
    fingerprint: text(read(value, "fingerprint")),
    name: text(read(value, "name")),
    status: text(read(value, "status")),
    severity: number(read(value, "severity")),
    linked_at: text(read(value, "linked_at", "linkedAt")),
  };
}

function numberOrText(value: unknown): number | string {
  if (typeof value === "number") return value;
  const candidate = text(value);
  const parsed = Number(candidate);
  return candidate !== "" && Number.isFinite(parsed) ? parsed : candidate;
}

function toRun(value: unknown): Run {
  return {
    id: number(read(value, "id")),
    incident_id: number(read(value, "incident_id", "incidentId")),
    mode: text(read(value, "mode")),
    status: text(read(value, "status")),
    retry_of: optionalNumber(read(value, "retry_of", "retryOf")),
    started_at: text(read(value, "started_at", "startedAt")),
    finished_at: optionalText(read(value, "finished_at", "finishedAt")),
    rca_text: optionalText(read(value, "rca_text", "rcaText", "rca_summary", "rcaSummary")),
    tokens_in: number(read(value, "tokens_in", "tokensIn")),
    tokens_out: number(read(value, "tokens_out", "tokensOut")),
  };
}

function optionalNumber(value: unknown): number | null | undefined {
  if (value === null) return null;
  if (value === undefined) return undefined;
  return number(value);
}

function toFlowNode(value: unknown, index: number): FlowNode {
  return {
    id: text(read(value, "id", "key", "name"), `node-${index}`),
    key: optionalText(read(value, "key")) ?? undefined,
    name: text(read(value, "name", "label"), `Stage ${index + 1}`),
    phase: optionalText(read(value, "phase")) ?? undefined,
    status: text(read(value, "status"), "queued"),
    started_at: optionalText(read(value, "started_at", "startedAt")),
    finished_at: optionalText(read(value, "finished_at", "finishedAt")),
    duration_ms: optionalNumber(read(value, "duration_ms", "durationMs")),
    timeout_ms: optionalNumber(read(value, "timeout_ms", "timeoutMs")),
    error: optionalText(read(value, "error")),
    owner: optionalText(read(value, "owner", "responsible")),
    retries: number(read(value, "retries", "retry_count", "retryCount")),
    step_id: optionalNumber(read(value, "step_id", "stepId")),
    run_id: optionalNumber(read(value, "run_id", "runId")),
  };
}

function toEvent(value: unknown): EventDTO {
  return {
    id: number(read(value, "id")),
    incident_id: number(read(value, "incident_id", "incidentId")),
    run_id: optionalNumber(read(value, "run_id", "runId")),
    approval_id: optionalNumber(read(value, "approval_id", "approvalId")),
    event_type: text(read(value, "event_type", "eventType", "type")),
    phase: text(read(value, "phase")),
    status: text(read(value, "status")),
    summary: text(read(value, "summary", "message")),
    payload: (parseJSON(read(value, "payload", "payload_json", "payloadJson")) as JsonObject | null) ?? null,
    created_at: text(read(value, "created_at", "createdAt")),
  };
}

function toProblem(value: unknown): ProblemDTO {
  return {
    id: number(read(value, "id")),
    incident_id: number(read(value, "incident_id", "incidentId")),
    run_id: optionalNumber(read(value, "run_id", "runId")),
    code: text(read(value, "code")),
    severity: text(read(value, "severity"), "warning"),
    status: text(read(value, "status"), "open"),
    summary: text(read(value, "summary")),
    detail: parseJSON(read(value, "detail", "detail_json", "detailJson")),
    first_seen_at: optionalText(read(value, "first_seen_at", "firstSeenAt")) ?? undefined,
    last_seen_at: optionalText(read(value, "last_seen_at", "lastSeenAt")) ?? undefined,
    resolved_at: optionalText(read(value, "resolved_at", "resolvedAt")),
  };
}

function toApproval(value: unknown): ApprovalDTO {
  const source = object(value);
  // Decision fields use the exact contract: never coerce unknown values into consent.
  const field = (key: string): string => typeof source[key] === "string" ? source[key] : "";
  const verification = object(source.verification);
  return {
    id: number(source.id),
    incident_id: number(source.incident_id),
    run_id: optionalNumber(source.run_id),
    parent_approval_id: optionalNumber(source.parent_approval_id),
    tool_name: field("tool_name"),
    plan_hash: field("plan_hash"),
    reason: field("reason"),
    kind: field("kind"),
    target: field("target"),
    target_id: field("target_id"),
    rule_id: field("rule_id"),
    rule_version: field("rule_version"),
    mode: field("mode"),
    revision: field("revision"),
    checks: Array.isArray(source.checks) ? source.checks.filter((value): value is string => typeof value === "string") : [],
    compensation: field("compensation"),
    result: source.result && typeof source.result === "object" ? toReceipt(source.result) : null,
    verification: {
      status: text(verification.status, "unknown"),
      phase: optionalText(verification.phase) ?? undefined,
      last_checked_at: optionalText(verification.last_checked_at),
      deadline_at: optionalText(verification.deadline_at),
      detail: optionalText(verification.detail) ?? undefined,
    },
    status: field("status"),
    expires_at: field("expires_at"),
    created_at: optionalText(read(value, "created_at", "createdAt")) ?? undefined,
    decided_by: optionalText(read(value, "decided_by", "decidedBy")),
    decided_at: optionalText(read(value, "decided_at", "decidedAt")),
    decision_reason: optionalText(read(value, "decision_reason", "decisionReason")),
    decision_source: optionalText(read(value, "decision_source", "decisionSource")),
  };
}

// Receipt is the structured execution result: whether the write happened.
export interface Receipt {
  written: boolean;
  outcome: string;
  before: string;
  after: string;
  detail: string;
  error: string;
  manual_check: boolean;
}

function toReceipt(value: unknown): Receipt {
  const source = object(value);
  return {
    written: source.written === true,
    outcome: text(source.outcome),
    before: text(source.before),
    after: text(source.after),
    detail: text(source.detail),
    error: text(source.error),
    manual_check: source.manual_check === true,
  };
}

function toConversationMessage(value: unknown): ConversationMessage {
  const citationValues = list(value, "citations");
  const actionValues = list(value, "suggested_actions");
  return {
    id: number(read(value, "id")),
    incident_id: number(read(value, "incident_id", "incidentId")),
    run_id: optionalNumber(read(value, "run_id", "runId")),
    reply_to_id: optionalNumber(read(value, "reply_to_id", "replyToId")),
    channel: text(read(value, "channel"), "web"),
    role: text(read(value, "role"), "user"),
    actor_id: optionalText(read(value, "actor_id", "actorId")),
    actor_name: optionalText(read(value, "actor_name", "actorName")),
    content: text(read(value, "content")),
    tool_name: optionalText(read(value, "tool_name", "toolName")),
    tool_call_id: optionalText(read(value, "tool_call_id", "toolCallId")),
    status: text(read(value, "status"), "queued"),
    citations: citationValues.map(toCitation),
    uncertainties: list(value, "uncertainties").map((item) => text(item)).filter(Boolean),
    suggested_actions: actionValues.map(toSuggestedAction),
    needs_user_input: read(value, "needs_user_input", "needsUserInput") === undefined ? undefined : boolean(read(value, "needs_user_input", "needsUserInput")),
    created_at: text(read(value, "created_at", "createdAt")),
    finished_at: optionalText(read(value, "finished_at", "finishedAt")),
  };
}

function toCitation(value: unknown): ConversationCitation {
	return {
		id: optionalNumber(read(value, "id")) ?? undefined,
		kind: optionalText(read(value, "kind")) ?? undefined,
		type: optionalText(read(value, "type")) ?? undefined,
		label: optionalText(read(value, "label")) ?? undefined,
		event_id: optionalNumber(read(value, "event_id", "eventId")) ?? undefined,
		step_id: optionalNumber(read(value, "step_id", "stepId")) ?? undefined,
		quote: optionalText(read(value, "quote")) ?? undefined,
		reference: optionalText(read(value, "reference")) ?? undefined,
	};
}

function toSuggestedAction(value: unknown): SuggestedAction {
  return {
    type: optionalText(read(value, "type")) ?? undefined,
    target: optionalText(read(value, "target")) ?? undefined,
    reason: optionalText(read(value, "reason")) ?? undefined,
    description: optionalText(read(value, "description")) ?? undefined,
  };
}

function toModelState(value: unknown): ModelState {
  return {
    current_model: text(read(value, "current_model", "currentModel")),
    updated_at: text(read(value, "updated_at", "updatedAt")),
    models: list(value, "models").map((item) => ({
      id: text(read(item, "id")),
      thinking_enabled: boolean(read(item, "thinking_enabled", "thinkingEnabled")),
    })).filter((item) => item.id !== ""),
  };
}

function unwrap(value: unknown, ...keys: string[]): unknown {
  for (const key of keys) {
    const candidate = read(value, key);
    if (candidate !== undefined) return candidate;
  }
  return value;
}

async function parseResponse(response: Response): Promise<unknown> {
  const raw = await response.text();
  if (!raw) return null;
  try {
    return JSON.parse(raw) as unknown;
  } catch {
    return raw;
  }
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  // The server requires this header on cookie-authenticated writes (CSRF).
  headers.set("X-Requested-With", "oncall-console");
  if (method !== "GET" && method !== "HEAD" && init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(path, { ...init, headers, credentials: "same-origin" });
  const body = await parseResponse(response);
  if (response.status === 401 && path !== "/api/v1/session") {
    window.dispatchEvent(new Event(unauthorizedEvent));
  }
  if (!response.ok) {
    const message = text(read(body, "error", "message"), response.statusText || "Request failed");
    throw new ApiError(response.status, message, body);
  }
  return body as T;
}

export async function getControlRoom(incidentID: number): Promise<ControlRoom> {
  const body = await request<unknown>(`/api/v1/incidents/${incidentID}/control-room`);
  const incident = toIncident(unwrap(body, "incident"));
  // 可选嵌套对象必须用 read：unwrap 在所有 key 都缺失时回退成整个响应体，
  // 于是「没有待审批」会被解析成一个字段全空的假审批单。
  const currentRunValue = read(body, "current_run", "currentRun");
  const currentRun = currentRunValue && Object.keys(object(currentRunValue)).length ? toRun(currentRunValue) : null;
  const approvalValue = read(body, "pending_approval", "pendingApproval");
  const latestActionValue = read(body, "latest_action");
  return {
    incident,
    members: list(body, "members").map(toMember),
    current_run: currentRun,
    flow_nodes: list(body, "flow_nodes").map(toFlowNode),
    open_problems: list(body, "open_problems").map(toProblem),
    pending_approval: approvalValue && Object.keys(object(approvalValue)).length ? toApproval(approvalValue) : null,
    latest_action: latestActionValue && Object.keys(object(latestActionValue)).length ? toApproval(latestActionValue) : null,
    recent_events: list(body, "recent_events").map(toEvent).filter((event) => event.id > 0),
  };
}

export async function listIncidents(status = "", limit = 50): Promise<Incident[]> {
  const query = new URLSearchParams({ limit: String(Math.min(100, Math.max(1, limit))) });
  if (status) query.set("status", status);
  const body = await request<unknown>(`/api/v1/control-room/incidents?${query.toString()}`);
  return list(body, "incidents").map(toIncident);
}

export async function getCurrentModel(): Promise<ModelState> {
  return toModelState(await request<unknown>("/api/v1/control-room/model"));
}

export async function switchCurrentModel(model: string): Promise<ModelState> {
  return toModelState(await request<unknown>("/api/v1/admin/model", {
    method: "PUT",
    body: JSON.stringify({ model }),
  }));
}

function toSession(value: unknown): Session {
  const role = text(read(value, "role"));
  return {
    id: text(read(value, "id")),
    name: text(read(value, "name")),
    role: role === "admin" || role === "operator" ? role : "viewer",
  };
}

export async function getSession(): Promise<Session> {
  return toSession(await request<unknown>("/api/v1/session"));
}

// login exchanges a personal token for an HttpOnly session cookie. The token is
// not stored by the browser.
export async function login(token: string): Promise<Session> {
  return toSession(await request<unknown>("/api/v1/session", {
    method: "POST",
    body: JSON.stringify({ token: token.trim() }),
  }));
}

export async function logout(): Promise<void> {
  await request<unknown>("/api/v1/session", { method: "DELETE" });
}

export async function getEvents(incidentID: number, after = 0, limit = 100): Promise<EventDTO[]> {
  const body = await request<unknown>(`/api/v1/incidents/${incidentID}/events?after=${Math.max(0, after)}&limit=${Math.min(100, Math.max(1, limit))}`);
  return list(body, "events").map(toEvent).filter((event) => event.id > 0);
}

export async function getRuns(incidentID: number, after = 0, limit = 100): Promise<Run[]> {
  const body = await request<unknown>(`/api/v1/incidents/${incidentID}/runs?after=${Math.max(0, after)}&limit=${Math.min(100, Math.max(1, limit))}`);
  return list(body, "runs").map(toRun);
}

export async function getRunSteps(runID: number, incidentID: number, after = 0, limit = 100): Promise<RunStep[]> {
	const base = `/api/v1/incidents/${incidentID}/runs/${runID}/steps`;
	const body = await request<unknown>(`${base}?after=${Math.max(0, after)}&limit=${Math.min(100, Math.max(1, limit))}`);
	return list(body, "steps").map(toRunStep);
}

function toRunStep(value: unknown): RunStep {
  const input = parseJSON(read(value, "input", "input_summary", "input_json", "inputJson"));
  const output = parseJSON(read(value, "output", "output_summary", "output_json", "outputJson"));
  return {
    id: number(read(value, "id")),
    run_id: number(read(value, "run_id", "runId")),
    seq: number(read(value, "seq")),
    kind: text(read(value, "kind")),
    name: text(read(value, "name")),
    status: optionalText(read(value, "status")) ?? undefined,
    input,
    output,
    error: optionalText(read(value, "error")),
    started_at: text(read(value, "started_at", "startedAt")),
    finished_at: optionalText(read(value, "finished_at", "finishedAt")),
    duration_ms: optionalNumber(read(value, "duration_ms", "durationMs")),
    owner: optionalText(read(value, "owner", "responsible")),
    truncated: read(value, "truncated") === undefined ? undefined : boolean(read(value, "truncated")),
  };
}

export async function getProblems(incidentID: number, status = "open", limit = 100): Promise<ProblemDTO[]> {
	const body = await request<unknown>(`/api/v1/incidents/${incidentID}/problems?status=${encodeURIComponent(status)}&limit=${Math.min(100, Math.max(1, limit))}`);
	return list(body, "problems").map(toProblem);
}

export async function getConversation(incidentID: number, after = 0, limit = 100): Promise<ConversationMessage[]> {
	const pageLimit = Math.min(100, Math.max(1, limit));
	const messages: ConversationMessage[] = [];
	let cursor = Math.max(0, after);
	for (let pageIndex = 0; pageIndex < 100; pageIndex += 1) {
		const body = await request<unknown>(`/api/v1/incidents/${incidentID}/conversation?after=${cursor}&limit=${pageLimit}`);
		const page = list(body, "messages").map(toConversationMessage);
		messages.push(...page);
		if (page.length < pageLimit) break;
		const next = page.reduce((value, item) => Math.max(value, item.id), cursor);
		if (next === cursor) break;
		cursor = next;
	}
	return messages;
}

export async function askQuestion(incidentID: number, question: string): Promise<ConversationMessage> {
  const body = await request<unknown>(`/api/v1/incidents/${incidentID}/questions`, {
    method: "POST",
    body: JSON.stringify({ question }),
  });
  return toConversationMessage(unwrap(body, "message"));
}

export async function rediagnose(incidentID: number): Promise<Run | null> {
  const body = await request<unknown>(`/api/v1/incidents/${incidentID}/rediagnose`, { method: "POST" });
  const value = unwrap(body, "run");
  return value && Object.keys(object(value)).length ? toRun(value) : null;
}

export async function requestEvidence(incidentID: number, requestText = "Collect additional safe evidence for this incident"): Promise<unknown> {
  return request(`/api/v1/incidents/${incidentID}/request-evidence`, {
    method: "POST",
    body: JSON.stringify({ request: requestText.slice(0, 4000) }),
  });
}

export async function decideApproval(approvalID: number, approve: boolean, planHash: string, reason: string): Promise<ApprovalDTO> {
  const body = await request<unknown>(`/api/v1/approvals/${approvalID}/${approve ? "approve" : "deny"}`, {
    method: "POST",
    body: JSON.stringify({ plan_hash: planHash, reason }),
  });
  return toApproval(unwrap(body, "approval"));
}

interface IncidentStreamOptions {
	after?: number;
	onEvent: (event: EventDTO) => void;
	onOpen?: () => void;
	onError?: () => void;
}

export function subscribeIncident(incidentID: number, options: IncidentStreamOptions): () => void {
	const after = Math.max(0, options.after ?? 0);
	const source = new EventSource(`/api/v1/incidents/${incidentID}/stream?after=${after}`, { withCredentials: true });
	source.onopen = () => options.onOpen?.();
	source.onerror = () => options.onError?.();
	const consume = (event: Event) => {
		const message = event as MessageEvent<string>;
		try {
			const body = JSON.parse(message.data) as unknown;
			const parsed = toEvent(unwrap(body, "event"));
			if (parsed.id > 0) options.onEvent(parsed);
		} catch {
			// Invalid events cannot safely update the control room.
		}
	};
	source.onmessage = consume;
	const eventNames = [
		"incident.created", "incident.promoted", "incident.resolved", "run.queued", "run.started", "run.succeeded", "run.failed", "run.stalled",
		"collector.started", "collector.completed", "collector.failed", "llm.started", "llm.tool_called", "llm.completed", "llm.failed",
		"guard.evaluated", "guard.overridden", "policy.evaluated", "policy.degraded", "approval.created", "approval.approved", "approval.denied", "approval.expired",
		"execution.started", "execution.completed", "execution.failed", "execution.aborted", "compensation.queued", "verify.queued", "verify.started", "verify.checked", "verify.passed", "verify.failed", "verify.inconclusive", "verify.stable", "verify.recurred", "retry.scheduled", "escalation.required", "review.recorded",
		"notification.sent", "notification.failed", "conversation.asked", "conversation.answered", "conversation.failed", "incident.event",
	];
	for (const eventName of eventNames) source.addEventListener(eventName, consume);
	return () => {
		for (const eventName of eventNames) source.removeEventListener(eventName, consume);
		source.close();
	};
}

// Remediation is the automatic-handling state: the rules release in effect,
// emergency stop, per-rule budget use and blocks, and the control audit.
export interface RemediationRule {
  id: string;
  action: string;
  mode: string;
  alerts: string[];
  max_executions: number;
  window_minutes: number;
  executions: number;
  blocked: string;
}

export interface ControlEvent {
  id: number;
  kind: string;
  rule_id: string;
  actor: string;
  reason: string;
  created_at: string;
}

export interface Remediation {
  service: string;
  env: string;
  rules_version: string;
  emergency_stop: boolean;
  stop_reason: string;
  maintenance: string;
  busy_with: number;
  rules: RemediationRule[];
  events: ControlEvent[];
}

function strings(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

export async function getRemediation(): Promise<Remediation> {
  const body = object(await request<unknown>("/api/v1/remediation"));
  return {
    service: text(body.service),
    env: text(body.env),
    rules_version: text(body.rules_version),
    emergency_stop: body.emergency_stop === true,
    stop_reason: text(body.stop_reason),
    maintenance: text(body.maintenance),
    busy_with: number(body.busy_with),
    rules: list(body, "rules").map((value) => {
      const rule = object(value);
      return {
        id: text(rule.id), action: text(rule.action), mode: text(rule.mode), alerts: strings(rule.alerts),
        max_executions: number(rule.max_executions), window_minutes: number(rule.window_minutes),
        executions: number(rule.executions), blocked: text(rule.blocked),
      };
    }),
    events: list(body, "events").map((value) => {
      const event = object(value);
      return { id: number(event.id), kind: text(event.kind), rule_id: text(event.rule_id), actor: text(event.actor), reason: text(event.reason), created_at: text(event.created_at) };
    }),
  };
}

// controlRemediation records an admin decision: stop, resume or a rule reset.
export async function controlRemediation(kind: "stop" | "resume" | "reset", reason: string, ruleID = ""): Promise<void> {
  const path = kind === "reset" ? `/api/v1/remediation/rules/${encodeURIComponent(ruleID)}/reset` : `/api/v1/remediation/${kind}`;
  await request(path, { method: "POST", body: JSON.stringify({ reason: reason.slice(0, 500) }) });
}

// Report figures are null when their denominator is zero: a small sample is
// never shown as a rate.
export interface Report {
  since: string;
  until: string;
  incidents: number;
  executions: { total: number; reviewed: number; wrong: number; coverage: number | null; error_rate: number | null };
  unattended: { recovered: number; confirmed: number; rate: number | null; confirmed_rate: number | null; in_scope: number; in_scope_rate: number | null };
  recovery_minutes: { median: number | null; max: number | null };
  recurrence: { watched: number; recurred: number; rate: number | null };
  manual: { incidents: number; share: number | null; minutes: number };
  cost: { tokens_in: number; tokens_out: number; tokens_per_incident: number | null };
  root_cause: { reviewed: number; correct: number; partial: number; wrong: number; unknown: number; accuracy: number | null };
  review_queue: { incident_id: number; approval_id: number; reason: string }[];
}

export async function getReport(days = 30): Promise<Report> {
  return request<Report>(`/api/v1/remediation/report?days=${Math.min(90, Math.max(1, days))}`);
}

export interface Review {
  id: number;
  incident_id: number;
  run_id?: number | null;
  approval_id?: number | null;
  subject: string;
  verdict: string;
  root_cause: string;
  actual_fix: string;
  manual_minutes: number;
  reviewer: string;
  created_at: string;
}

function toReview(value: unknown): Review {
  const source = object(value);
  return {
    id: number(source.id), incident_id: number(source.incident_id), run_id: optionalNumber(source.run_id), approval_id: optionalNumber(source.approval_id),
    subject: text(source.subject), verdict: text(source.verdict), root_cause: text(source.root_cause), actual_fix: text(source.actual_fix),
    manual_minutes: number(source.manual_minutes), reviewer: text(source.reviewer), created_at: text(source.created_at),
  };
}

export async function getReviews(incidentID: number): Promise<Review[]> {
  return list(await request<unknown>(`/api/v1/incidents/${incidentID}/reviews`), "reviews").map(toReview);
}

export interface ReviewInput {
  subject: "diagnosis" | "action";
  verdict: "correct" | "partial" | "wrong" | "unknown";
  run_id?: number;
  approval_id?: number;
  root_cause: string;
  actual_fix: string;
  manual_minutes: number;
}

export async function addReview(incidentID: number, input: ReviewInput): Promise<Review> {
  return toReview(await request<unknown>(`/api/v1/incidents/${incidentID}/reviews`, { method: "POST", body: JSON.stringify(input) }));
}

export interface Change {
  id: number;
  change_type: string;
  release_id: string;
  image_ref: string;
  db_migration: string;
  verified_at?: string | null;
  occurred_at: string;
  source: string;
  actor: string;
}

export async function getChanges(): Promise<Change[]> {
  return list(await request<unknown>("/api/v1/changes"), "changes").map((value) => {
    const source = object(value);
    return {
      id: number(source.id), change_type: text(source.change_type), release_id: text(source.release_id), image_ref: text(source.image_ref),
      db_migration: text(source.db_migration), verified_at: optionalText(source.verified_at), occurred_at: text(source.occurred_at),
      source: text(source.source), actor: text(source.actor),
    };
  });
}

export async function verifyRelease(changeID: number): Promise<void> {
  await request(`/api/v1/changes/${changeID}/verify`, { method: "POST" });
}

// listApprovals feeds the overview's decision queue; status is the server filter
// (for example "pending"). Snapshot fields follow the same exact contract as the
// control room, so an unverifiable snapshot is shown incomplete, never inferred.
export async function listApprovals(status = "pending"): Promise<ApprovalDTO[]> {
  const query = status ? `?status=${encodeURIComponent(status)}` : "";
  return list(await request<unknown>(`/api/v1/approvals${query}`), "approvals").map(toApproval);
}
