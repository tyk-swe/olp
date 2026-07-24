import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const base = __ENV.OLP_QUALIFICATION_ORIGIN;
const key = __ENV.OLP_QUALIFICATION_API_KEY;
const model = __ENV.OLP_QUALIFICATION_MODEL || 'sdk-smoke-route';
const profile = __ENV.OLP_QUALIFICATION_PROFILE || 'load';
const duration = __ENV.OLP_QUALIFICATION_DURATION || (profile === 'soak' ? '30m' : '2m');
const warmup = __ENV.OLP_QUALIFICATION_WARMUP || '30s';
const rate = Number(__ENV.OLP_QUALIFICATION_RATE || (profile === 'soak' ? 50 : 100));
const headers = { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' };

const unaryLatency = new Trend('olp_unary_latency', true);
const countLatency = new Trend('olp_count_latency', true);
const discoveryLatency = new Trend('olp_discovery_latency', true);
const streamTtfb = new Trend('olp_stream_ttfb', true);
const failedChecks = new Counter('olp_failed_checks');

export const options = {
  discardResponseBodies: false,
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate',
      rate: Math.max(1, Math.floor(rate / 10)),
      timeUnit: '1s',
      duration: warmup,
      preAllocatedVUs: 20,
      maxVUs: 100,
      exec: 'exercise'
    },
    qualification: {
      executor: 'constant-arrival-rate',
      rate,
      timeUnit: '1s',
      duration,
      startTime: warmup,
      preAllocatedVUs: profile === 'soak' ? 100 : 200,
      maxVUs: profile === 'soak' ? 400 : 800,
      exec: 'exercise',
      tags: { phase: 'qualification' }
    }
  },
  thresholds: {
    checks: [{ threshold: 'rate==1', abortOnFail: false }],
    dropped_iterations: ['count==0'],
    'olp_failed_checks': ['count==0'],
    'olp_unary_latency{phase:qualification}': ['p(95)<15', 'p(99)<30'],
    'olp_count_latency{phase:qualification}': ['p(95)<15', 'p(99)<30'],
    'olp_discovery_latency{phase:qualification}': ['p(95)<15', 'p(99)<30'],
    'olp_stream_ttfb{phase:qualification}': ['p(95)<15', 'p(99)<30']
  }
};

function accepted(response, predicate, label) {
  const ok = check(response, { [label]: (value) => value.status === 200 && predicate(value) });
  if (!ok) failedChecks.add(1);
}

export function exercise() {
  const pick = (__ITER * 37 + __VU * 17) % 100;
  if (pick < 55) {
    const response = http.post(`${base}/openai/v1/chat/completions`, JSON.stringify({
      model, max_tokens: 8, messages: [{ role: 'user', content: 'qualification unary' }]
    }), { headers });
    unaryLatency.add(response.timings.duration);
    accepted(response, (value) => value.json('choices.0.message.content').length > 0, 'unary response');
  } else if (pick < 80) {
    const response = http.post(`${base}/openai/v1/chat/completions`, JSON.stringify({
      model, max_tokens: 8, stream: true,
      messages: [{ role: 'user', content: 'qualification stream' }]
    }), { headers });
    streamTtfb.add(response.timings.waiting);
    accepted(response, (value) => value.body.includes('data: [DONE]'), 'stream completion');
  } else if (pick < 90) {
    const response = http.post(`${base}/openai/v1/responses/input_tokens`, JSON.stringify({
      model, input: 'qualification token count'
    }), { headers });
    countLatency.add(response.timings.duration);
    accepted(response, (value) => value.json('input_tokens') > 0, 'positive token count');
  } else {
    const response = http.get(`${base}/openai/v1/models`, { headers });
    discoveryLatency.add(response.timings.duration);
    accepted(response, (value) => value.json('data').some((item) => item.id === model), 'model discovery');
  }
}

export function handleSummary(data) {
  const output = __ENV.OLP_QUALIFICATION_SUMMARY || 'qualification-summary.json';
  return { [output]: JSON.stringify(data, null, 2), stdout: `qualification profile ${profile} complete\n` };
}
