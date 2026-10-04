// The harness contract for client suites: the gateway, the key, the model names
// and the recording of what the scripted upstream received. The environment
// variables are published by tests/clientfixture and documented in
// tests/clients/README.md. Suites run through tests/clients/run.sh; without it
// every import fails loudly instead of testing nothing.
import assert from 'node:assert/strict';
import { setTimeout as sleep } from 'node:timers/promises';

function required(name) {
  const value = process.env[name];
  assert.ok(value, `${name} is not set; run the suites through tests/clients/run.sh`);
  return value;
}

export const origin = required('OLP_CLIENTS_ORIGIN');
export const apiKey = required('OLP_CLIENTS_API_KEY');
export const restrictedApiKey = required('OLP_CLIENTS_RESTRICTED_API_KEY');
export const upstreamURL = required('OLP_CLIENTS_UPSTREAM_URL');
export const defaultReply = required('OLP_CLIENTS_DEFAULT_REPLY');
export const toolResultsPrefix = required('OLP_CLIENTS_TOOL_RESULTS_PREFIX');

/** Base URLs as each vendor's SDK expects them. */
export const baseURLs = {
  openai: required('OLP_CLIENTS_OPENAI_BASE_URL'),
  anthropic: required('OLP_CLIENTS_ANTHROPIC_BASE_URL'),
  gemini: required('OLP_CLIENTS_GEMINI_BASE_URL')
};

/** Route slugs: the model names a client sends. */
export const models = {
  openai: required('OLP_CLIENTS_MODEL_OPENAI'),
  anthropic: required('OLP_CLIENTS_MODEL_ANTHROPIC'),
  gemini: required('OLP_CLIENTS_MODEL_GEMINI'),
  openaiStrict: required('OLP_CLIENTS_MODEL_OPENAI_STRICT'),
  anthropicStrict: required('OLP_CLIENTS_MODEL_ANTHROPIC_STRICT'),
  geminiStrict: required('OLP_CLIENTS_MODEL_GEMINI_STRICT'),
  geminiEmbedStrict: required('OLP_CLIENTS_MODEL_GEMINI_EMBED_STRICT')
};

/** The model names the upstream sees after the gateway rewrites a slug. */
export const upstreamModels = {
  openai: required('OLP_CLIENTS_UPSTREAM_MODEL_OPENAI'),
  anthropic: required('OLP_CLIENTS_UPSTREAM_MODEL_ANTHROPIC'),
  gemini: required('OLP_CLIENTS_UPSTREAM_MODEL_GEMINI')
};

const nativeFetch = globalThis.fetch.bind(globalThis);

/**
 * A fetch that only reaches the gateway, for SDKs that take one. A client that
 * wanders elsewhere fails the test instead of leaving the machine.
 */
export async function localFetch(input, init) {
  const url = new URL(input instanceof Request ? input.url : String(input));
  assert.equal(url.origin, origin, `client attempted a request outside OLP: ${url.origin}`);
  return nativeFetch(input, init);
}

/**
 * Directives that script the upstream from inside a prompt. The grammar is
 * documented in tests/clientfixture/scripted/script.go.
 */
export const script = {
  tool: (name, args = {}) => `[[olp:tool ${name} ${JSON.stringify(args)}]]`,
  also: (name, args = {}) => `[[olp:also ${name} ${JSON.stringify(args)}]]`,
  reply: (text) => `[[olp:reply ${JSON.stringify(text)}]]`,
  think: (text) => `[[olp:think ${JSON.stringify(text)}]]`,
  fail: (status) => `[[olp:fail ${status}]]`
};

/** What the final text is after a tool loop returned these results. */
export const afterTools = (...results) => toolResultsPrefix + results.join(' | ');

/**
 * The requests the upstream received, in arrival order. Filters narrow by
 * `dialect` or `path` prefix, exact `model` and exact `script`. It waits for
 * requests still in flight, so a stream the client has finished reading is
 * complete when this returns.
 */
export async function recorded(filter = {}) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const response = await nativeFetch(`${upstreamURL}/__recorded?${new URLSearchParams(filter)}`);
    assert.equal(response.status, 200);
    const { requests, in_flight: inFlight } = await response.json();
    if (inFlight === 0) return requests;
    await sleep(20);
  }
  assert.fail('upstream requests are still in flight');
}

/** Forget the recording, the stored responses and the prompt cache. */
export async function resetRecorded() {
  const response = await nativeFetch(`${upstreamURL}/__recorded`, { method: 'DELETE' });
  assert.equal(response.status, 204);
}

/**
 * Assert that every upstream request was authenticated with the upstream
 * credential, never carried a caller credential, and succeeded unless the test
 * expects otherwise. Returns the requests for further assertions.
 */
export function assertClean(requests, { status = 200 } = {}) {
  for (const request of requests) {
    assert.equal(request.authorized, true, `upstream refused ${request.path}`);
    assert.equal(request.leaked_client_credential, false, `${request.path} carried a caller credential`);
    assert.equal(request.status, status, `${request.path} answered ${request.status}`);
  }
  return requests;
}

/** The only upstream request of a test, checked with {@link assertClean}. */
export async function onlyRequest(filter) {
  const requests = assertClean(await recorded(filter));
  assert.equal(requests.length, 1, `expected one upstream request, saw ${requests.map((r) => r.path)}`);
  return requests[0];
}
