# Frontend tests

```sh
cd web
npm ci
npx playwright install chromium
npm test
npm run typecheck
npm run build
```

The default test server is a new Vite process on `127.0.0.1:15173`, with `--strictPort` and `reuseExistingServer: false`. It never reuses an existing service. All browser test API traffic is intercepted in `fixtures.ts`; unexpected requests fail the test. The SSE tests send actual `text/event-stream` responses to the browser's native EventSource. Contract tests call the frontend API adapter with an in-process fetch stub.

`PLAYWRIGHT_BASE_URL=http://127.0.0.1:<isolated-test-port> npm test` selects another **isolated test deployment** and skips starting Vite. Existing specs still intercept API responses; selecting a Go URL does not turn them into backend integration tests. Separate real-backend specs can use the same config/base URL without the `openRoom` fixture. Never select a live service.

## Contracts exercised

- Approval fields: `tool_name`, `kind`, `target`, `target_id`, `rule_id`, `rule_version`, `mode`, `revision`, `checks`, `compensation`, `reason`, `plan_hash`, `expires_at`; no `risk` alias. Only a complete `mode=manual` snapshot can be approved; missing target identity, rule, checks or hash visibly blocks approval.
- Both decision endpoints receive `{ plan_hash, reason }`. Hash comes from the clicked snapshot. Blank approval remarks remain allowed; denial requires a remark. Concurrent clicks do not issue duplicate requests. Expired snapshots cannot be submitted; HTTP 409 is shown and triggers an authoritative refresh.
- `pending_approval` is independent of `latest_action`. Latest action shows the receipt (`written`/`not_written`/`unknown`), the compensation, and `verification: { status, phase, last_checked_at?, deadline_at?, detail? }` with the watch phase (`stable`/`recurred`) as its own fact. Legacy records and unverifiable snapshots remain `unknown`; the frontend does not derive these states or infer recovery from run success.
- Named SSE events (execution, compensation, `verify.*` including `verify.stable`/`verify.recurred`, `review.recorded`) refresh the aggregate, including when `pending_approval` is null.
- `/remediation`: rules with mode, budget use and block reason; emergency stop/resume and rule reset only for admins and only with a reason. `/report`: rates always shown with raw counts ("样本不足" on an empty denominator) and the review queue. `/changes`: an operator marks a release healthy; a viewer cannot.
- Console shell (`console.spec.ts`): model-written Markdown keeps formatting but renders no raw HTML, images or `javascript:` links; a flow node opens the diagnosis trace with that step expanded and sensitive keys redacted; the overview lists firing incidents and pending decisions, and one failed section does not hide the others; the incident list filters by server status and locally by text; `⌘K` jumps to an incident. The shell polls `/api/v1/control-room/incidents`, so fixtures answer it.
- Monitor (`monitor.spec.ts`): the page renders the real embedded dashboard JSON; every target is one `POST /api/v1/prometheus/query_range` with the same `start`/`end`/`step`; value mappings show UP/DOWN as text; a failing query stays inside its panel; Loki panels are not rendered; board tabs switch dashboards; a custom window from the URL is queried as-is; an incident links to its own window.
- Incident reviews: an operator records the verdict, root cause, fix and manual minutes; the page never sends a reviewer; viewers see reviews without the form.

These tests do not validate backend authorization, transactions, execution, probe scheduling, or real MySQL behavior.
