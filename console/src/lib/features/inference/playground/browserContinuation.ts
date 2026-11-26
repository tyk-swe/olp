import { isJsonObject, parseJsonObject } from '#lib/editing/json.js';
import { gatewayFetch } from '#lib/management/gateway.js';
import {
  nativeObject,
  replaceNative,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '#lib/nativeJson.js';
import {
  assembleContinuation,
  continuationVersion,
  extractNativeUsage,
  isValidHandle,
  observationsFrom,
  parseAssistant,
  parseContinuationChunkValue,
  parseNativeDelivery,
  readContinuationFrames,
  requireObject,
  requireString,
  validHandle,
  type Assistant,
  type Observation,
  type ToolCall
} from './continuationStream';
import {
  fail,
  MAX_PUBLIC_REQUEST_BYTES,
  MAX_PUBLIC_RESPONSE_BYTES,
  parseNativeValue,
  publicGatewayHeaders,
  unwrapOutcome,
  utf8ByteLength,
  validateApiKey,
  type Outcome
} from './publicGateway';

export { continuationVersion, type Assistant, type Observation, type ToolCall };

export type ReadyTurn = {
  submission: string;
  handle: string;
  request: NativeObject;
  assistant: Assistant;
  observations: Observation[];
  finish: string;
  nativeUsageRaw?: string;
};

const submissionPattern =
  /^\d{1,16}\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const namedErrors: Readonly<Record<string, string | undefined>> = {
  policy_conflict: 'This key needs provider-state permission.',
  route_forbidden: 'This key cannot use the selected route.',
  state_carrier: 'The route does not admit this continuation contract.',
  continuation_mismatch: 'The prior handle does not match this history.',
  continuation_unavailable: 'The prior continuation is unavailable.',
  continuation_outcome_unknown: 'Accepted work has no recoverable result yet.',
  invalid_submission_identity: 'The submission identity is invalid or expired.'
};

export function nativeChatRequest(
  source: string,
  route: string
): Outcome<NativeObject> {
  const parsed = parseNativeValue(
    source,
    'The Chat request must be valid JSON.'
  );
  if (!parsed.ok) return parsed;
  if (!nativeObject(parsed.value))
    return fail('The Chat request must be a JSON object.');
  const model = parsed.value.model;
  if (model !== undefined && model !== '' && model !== route)
    return fail('The native request names another route.');
  if (
    !Array.isArray(parsed.value.messages) ||
    parsed.value.messages.length === 0
  )
    return fail('The native Chat request needs ordered messages.');
  if (!Array.isArray(parsed.value.tools) || parsed.value.tools.length === 0)
    return fail('The negotiated tool client needs a nonempty tools array.');
  const bound = replaceNative(parsed.value, ['model'], route);
  return nativeObject(bound)
    ? { ok: true, value: bound }
    : fail('The Chat request must be a JSON object.');
}

export function submissionID(now = Date.now()): string {
  if (!Number.isSafeInteger(now) || now < 0)
    throw new Error('Invalid submission time.');
  return `${now}.${crypto.randomUUID()}`;
}

function continuationHeaders(
  key: string,
  submission: string,
  handle?: string
): Outcome<Headers> {
  const validKey = validateApiKey(key);
  if (!validKey.ok) return validKey;
  if (!submissionPattern.test(submission))
    return fail('Invalid submission identity.');
  if (handle !== undefined && !isValidHandle(handle))
    return fail('Incomplete continuation delivery.');
  return {
    ok: true,
    value: publicGatewayHeaders(validKey.value, {
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      'X-OLP-Continuation': continuationVersion,
      'X-OLP-Submission-ID': submission,
      ...(handle ? { 'X-OLP-Continuation-Handle': handle } : {})
    })
  };
}

async function fetchContinuation(
  path: string,
  key: string,
  submission: string,
  opts: { body?: string; limit?: string; handle?: string; signal?: AbortSignal }
): Promise<Response> {
  const headers = unwrapOutcome(
    continuationHeaders(key, submission, opts.handle)
  );
  if (
    opts.body !== undefined &&
    utf8ByteLength(opts.body) > MAX_PUBLIC_REQUEST_BYTES
  )
    throw new Error(
      opts.limit ?? 'The native request exceeds the public body limit.'
    );
  const res = await gatewayFetch(path, {
    method: opts.body === undefined ? 'GET' : 'POST',
    headers,
    body: opts.body,
    signal: opts.signal
  });
  if (res.ok) return res;
  const parsed = parseJsonObject(
    (await res.text().catch(() => '')).slice(0, 8192)
  );
  const err = parsed && isJsonObject(parsed.error) ? parsed.error : null;
  const code = err && typeof err.code === 'string' ? err.code : '';
  throw new Error(
    namedErrors[code] ?? `Public inference request failed (${res.status}).`
  );
}

async function readNative(
  res: Response,
  limitMsg: string
): Promise<NativeObject> {
  const source = await res.text();
  if (utf8ByteLength(source) > MAX_PUBLIC_RESPONSE_BYTES)
    throw new Error(limitMsg);
  return parseNativeDelivery(source, MAX_PUBLIC_RESPONSE_BYTES);
}

export async function streamTurn(
  key: string,
  request: NativeObject,
  submission: string,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const body = stringifyNativeJSON(replaceNative(request, ['stream'], true));
  const res = await fetchContinuation('/v1/chat/completions', key, submission, {
    body,
    signal
  });
  return {
    submission,
    request,
    ...assembleContinuation(await readContinuationFrames(res))
  };
}

export function nextTurn(
  completed: ReadyTurn,
  results: readonly { tool_call_id: string; content: string }[]
): Outcome<NativeObject> {
  const calls = completed.assistant.tool_calls;
  if (!calls?.length || results.length !== calls.length)
    return fail('Provide one result for every ready tool call.');
  const messages = completed.request.messages;
  if (!Array.isArray(messages))
    return fail('The original messages are unavailable.');
  const matches = results.every(
    (r, i) => r.tool_call_id === calls[i]?.id && typeof r.content === 'string'
  );
  if (!matches) return fail('Tool results must match call order and identity.');
  const tools = results.map(({ tool_call_id, content }) => ({
    role: 'tool',
    tool_call_id,
    content
  }));
  const withMsgs = replaceNative(
    completed.request,
    ['messages'],
    [...messages, completed.assistant, ...tools]
  );
  const next = replaceNative(
    replaceNative(withMsgs, ['stream'], undefined),
    ['stream_options'],
    undefined
  );
  return nativeObject(next)
    ? { ok: true, value: next }
    : fail('The next Chat request must be a JSON object.');
}

function completedUnary(
  source: NativeValue,
  request: NativeObject,
  submission: string
): ReadyTurn {
  const payload = requireObject(source);
  const ext = requireObject(payload.olp);
  if (ext.version !== continuationVersion || ext.ready !== true)
    throw new Error('The result has no ready continuation.');
  const choices = payload.choices;
  if (!Array.isArray(choices) || choices.length !== 1)
    throw new Error('The result has an unsupported candidate count.');
  const selected = requireObject(choices[0]);
  return {
    submission,
    handle: validHandle(ext.handle),
    request,
    assistant: parseAssistant(selected.message),
    observations:
      ext.observations === undefined ? [] : observationsFrom(ext.observations),
    finish: requireString(selected.finish_reason),
    nativeUsageRaw: extractNativeUsage(payload)
  };
}

export async function unaryTurn(
  key: string,
  request: NativeObject,
  submission: string,
  parentHandle: string,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const res = await fetchContinuation('/v1/chat/completions', key, submission, {
    body: stringifyNativeJSON(request),
    limit: 'The next native request exceeds the public body limit.',
    handle: parentHandle,
    signal
  });
  return completedUnary(
    await readNative(res, 'The continuation result exceeds the client limit.'),
    request,
    submission
  );
}

export async function recoverTurn(
  key: string,
  submission: string,
  request: NativeObject,
  signal?: AbortSignal
): Promise<ReadyTurn> {
  const path = `/v1/continuation-submissions/${encodeURIComponent(submission)}`;
  const res = await fetchContinuation(path, key, submission, { signal });
  const recovery = await readNative(
    res,
    'Recovery result exceeds the client limit.'
  );
  if (recovery.version !== continuationVersion || recovery.state !== 'ready')
    throw new Error('The submission has no ready delivery.');
  const delivery = requireObject(recovery.delivery);
  const completed =
    delivery.stream === true && Array.isArray(delivery.frames)
      ? {
          submission,
          request,
          ...assembleContinuation(
            delivery.frames.map(parseContinuationChunkValue)
          )
        }
      : delivery.stream === false && delivery.body !== undefined
        ? completedUnary(delivery.body, request, submission)
        : null;
  if (!completed)
    throw new Error('This submission has another delivery shape.');
  if (
    stringifyNativeJSON(parseAssistant(recovery.assistant)) !==
    stringifyNativeJSON(completed.assistant)
  )
    throw new Error('Recovered assistant does not match the committed stream.');
  if (completed.handle !== validHandle(recovery.handle))
    throw new Error('Recovered handle does not match the committed stream.');
  return completed;
}
