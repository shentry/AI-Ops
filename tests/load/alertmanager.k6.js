// k6 v0.57.0: bounded, open-model load against the isolated oncall benchmark only.
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

const baseURL = __ENV.BASE_URL || 'http://server:18080';
const readPath = '/api/v1/control-room/incidents?limit=20';
const token = __ENV.AUTH_TOKEN;
const runID = __ENV.RUN_ID;
const mode = __ENV.MODE || 'duplicate';
const rate = Number(__ENV.RATE || 5);
const seconds = Number(__ENV.SECONDS || 20);
const batchSize = Number(__ENV.BATCH_SIZE || 1);
const startsAt = __ENV.STARTS_AT;
if (__ENV.BENCHMARK_ACK !== 'oncall-isolated-only' || !token || !runID || !startsAt) {
  throw new Error('Explicit benchmark acknowledgement, token, run ID and STARTS_AT are required');
}
if (!/^http:\/\/(server:18080|127\.0\.0\.1:18081)$/.test(baseURL)) {
  throw new Error('Only the isolated benchmark container or its loopback port is allowed');
}
if (!['duplicate', 'unique'].includes(mode) || !Number.isInteger(rate) || rate < 1 || rate > 250
    || !Number.isInteger(seconds) || seconds < 5 || seconds > 60
    || !Number.isInteger(batchSize) || batchSize < 1 || batchSize > 10) {
  throw new Error('Load is bounded to 250 webhook/s, 60s and 10 alerts/webhook');
}

const accepted = new Counter('webhooks_accepted');
const alertsAccepted = new Counter('alerts_accepted');
const unavailable = new Counter('webhooks_503');
const rejected = new Counter('webhooks_other_errors');
const acceptOK = new Rate('webhook_ok');
const acceptLatency = new Trend('webhook_latency', true);
const readOK = new Rate('read_ok');
const readLatency = new Trend('read_latency', true);

export const options = {
  discardResponseBodies: true,
  setupTimeout: '10s',
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
  scenarios: {
    alerts: {
      executor: 'constant-arrival-rate', exec: 'sendAlert',
      rate, timeUnit: '1s', duration: `${seconds}s`,
      preAllocatedVUs: 16, maxVUs: 64, gracefulStop: '5s',
    },
    control_reads: {
      executor: 'constant-arrival-rate', exec: 'readIncidents',
      rate: 1, timeUnit: '1s', duration: `${seconds}s`,
      preAllocatedVUs: 2, maxVUs: 4, gracefulStop: '5s',
    },
  },
  thresholds: {
    webhook_ok: [{ threshold: 'rate>0.95', abortOnFail: true, delayAbortEval: '10s' }],
    webhook_latency: [{ threshold: 'p(95)<2000', abortOnFail: true, delayAbortEval: '10s' }],
    read_ok: [{ threshold: 'rate>0.95', abortOnFail: true, delayAbortEval: '10s' }],
    read_latency: [{ threshold: 'p(95)<2000', abortOnFail: true, delayAbortEval: '10s' }],
    // A dropped iteration means the generator did not sustain the offered load.
    dropped_iterations: ['count==0'],
  },
};

const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };

export function setup() {
  const response = http.get(`${baseURL}/metrics`, { timeout: '5s', tags: { name: 'readiness' } });
  if (response.status !== 200) throw new Error('Benchmark server is not ready');
}

export function sendAlert() {
  const identity = mode === 'duplicate' ? 'same' : `${__VU}-${__ITER}`;
  const service = `k6-${runID}-${identity}`;
  const alerts = [];
  for (let i = 0; i < batchSize; i += 1) {
    alerts.push({
      status: 'firing',
      labels: {
        alertname: 'K6SyntheticAlert', service, instance: `synthetic-${identity}-${i}`,
        severity: 'info', env: 'isolated-benchmark',
      },
      // Keep the duplicate case byte-stable; dynamic annotations defeat dedup.
      annotations: { summary: 'Synthetic ingress load; no remediation target' },
      startsAt, endsAt: '0001-01-01T00:00:00Z',
    });
  }
  const response = http.post(`${baseURL}/webhook/alertmanager`, JSON.stringify({
    version: '4', receiver: 'k6-isolated', status: 'firing',
    groupLabels: { service }, commonLabels: { env: 'isolated-benchmark' }, alerts,
  }), { headers, timeout: '3s', tags: { name: 'alertmanager_webhook' } });
  const ok = response.status === 202;
  acceptOK.add(ok);
  acceptLatency.add(response.timings.duration);
  check(response, { 'webhook persisted (202)': () => ok });
  if (ok) {
    accepted.add(1);
    alertsAccepted.add(batchSize);
  } else if (response.status === 503) {
    unavailable.add(1);
  } else {
    rejected.add(1);
  }
}

export function readIncidents() {
  const response = http.get(`${baseURL}${readPath}`, {
    headers, timeout: '3s', tags: { name: 'incident_list' },
  });
  readOK.add(response.status === 200);
  readLatency.add(response.timings.duration);
  check(response, { 'incident list available': r => r.status === 200 });
}

export function handleSummary(data) {
  data.benchmark = {
    runID, mode, offeredWebhookRate: rate, durationSeconds: seconds, batchSize,
    offeredAlertRate: rate * batchSize, startsAt, readPath,
    scope: 'HTTP acceptance + MySQL ingest; info severity skips LLM diagnosis',
  };
  const value = name => data.metrics[name]?.values || {};
  return {
    [`/results/${runID}.summary.json`]: JSON.stringify(data, null, 2),
    stdout: JSON.stringify({
      runID, accepted: value('webhooks_accepted').count || 0,
      webhookP95Ms: value('webhook_latency')['p(95)'],
      readP95Ms: value('read_latency')['p(95)'],
      dropped: value('dropped_iterations').count || 0,
      webhookSuccessRate: value('webhook_ok').rate,
    }) + '\n',
  };
}
