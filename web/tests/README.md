# Frontend execution-trust tests

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

- Approval fields: `tool_name`, `target`, `scope=single_container`, `safety_level=L2|L3`, boolean `dry_run`, `reason`, `plan_hash`, `expires_at`; no `risk` alias. Missing/invalid required facts visibly block approval. Legacy/invalid `dry_run` remains unknown, never `false`.
- Both decision endpoints receive `{ plan_hash, reason }`. Hash comes from the clicked snapshot. Blank approval remarks remain allowed; denial requires a remark. Concurrent clicks do not issue duplicate requests. Expired snapshots cannot be submitted; HTTP 409 is shown and triggers an authoritative refresh.
- `pending_approval` is independent of `latest_action`. Latest action includes `verification: { status, last_checked_at?, deadline_at?, detail? }`. For trusted modern snapshots without a task, the server supplies `not_started` while pending/approved/executing and `not_applicable` after simulated/denied/expired/failed. A successful execution missing its required task, legacy records, and unverifiable snapshots remain `unknown`. Failed actions still require manual target checking; “not applicable” is not a recovery claim. The frontend does not derive these states or infer recovery from run success.
- Named SSE: `execution.simulated`, `verify.queued`, `verify.started`, `verify.checked`, `verify.passed`, `verify.failed`, `verify.inconclusive` refresh the aggregate, including when `pending_approval` is null.

These tests do not validate backend authorization, transactions, execution, probe scheduling, or real MySQL behavior.
