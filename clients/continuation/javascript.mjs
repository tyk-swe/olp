import { randomUUID } from 'node:crypto';

export const CONTINUATION_VERSION = 'chat-anthropic-tools-v1';

// The carrier admits at most 1024 ordered native blocks, so at most that many
// tool calls and claimed actions can correspond.
const MAX_CALLS = 1024;
const HANDLE_PATTERN = /^continuation_[0-9a-f]{32}$/;

// Optional SDK message members that sit outside the canonical assistant
// representation. Absent, null or empty values are dropped deliberately; a
// non-empty one is an incompatible delivery, never silently normalized away.
const SDK_MESSAGE_EXTRAS = new Set(['refusal', 'annotations', 'audio', 'function_call']);
const ASSISTANT_MEMBERS = new Set(['role', 'content', 'tool_calls', ...SDK_MESSAGE_EXTRAS]);
const TOOL_CALL_MEMBERS = new Set(['id', 'type', 'function']);
const TOOL_FUNCTION_MEMBERS = new Set(['name', 'arguments']);
const ACTION_MEMBERS = new Set(['tool_calls']);

export function submissionID(now = Date.now()) {
  if (!Number.isSafeInteger(now) || now < 0) throw new TypeError('Invalid submission time');
  return `${now}.${randomUUID()}`;
}

export function continuationHeaders(submission, handle) {
  if (!/^\d{1,16}\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(submission)) {
    throw new TypeError('Use a timestamp.UUID submission identity');
  }
  const headers = { 'X-OLP-Continuation': CONTINUATION_VERSION, 'X-OLP-Submission-ID': submission };
  if (handle !== undefined) {
    if (!HANDLE_PATTERN.test(handle)) throw new TypeError('Invalid continuation handle');
    headers['X-OLP-Continuation-Handle'] = handle;
  }
  return headers;
}

function checkExtension(extension) {
  if (!extension || extension.version !== CONTINUATION_VERSION) {
    throw new Error('The SDK did not retain the negotiated OLP continuation extension');
  }
}

function checkSDK(client) {
  if (client?.getUserAgent?.() !== 'OpenAI/JS 7.4.0' || typeof client?.chat?.completions?.create !== 'function') {
    throw new Error('This helper supports the qualified OpenAI JavaScript SDK 7.4.0 only');
  }
}

function record(value, what = 'continuation delivery') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`Malformed ${what}`);
  }
  return value;
}

function requiredString(value, what) {
  if (typeof value !== 'string') throw new Error(`Incomplete ${what}`);
  return value;
}

// Normalize one tool call to the exact carrier shape. The argument JSON is
// kept as its original string — never decoded into floating-point values and
// re-encoded — but it must be a complete native object before the call may
// correspond to an action.
function toolCallFrom(raw) {
  const call = record(raw, 'assistant tool call');
  for (const key of Object.keys(call)) {
    if (!TOOL_CALL_MEMBERS.has(key)) throw new Error('Unknown assistant tool call field');
  }
  if (call.type !== 'function' || typeof call.id !== 'string' || !call.id) {
    throw new Error('Unsupported assistant tool call');
  }
  const fn = record(call.function, 'assistant tool function');
  for (const key of Object.keys(fn)) {
    if (!TOOL_FUNCTION_MEMBERS.has(key)) throw new Error('Unknown assistant tool function field');
  }
  const name = fn.name;
  const args = fn.arguments;
  if (typeof name !== 'string' || !name || typeof args !== 'string' || !args) {
    throw new Error('Incomplete assistant tool call');
  }
  let parsed;
  try {
    parsed = JSON.parse(args);
  } catch {
    // Partial arguments stay observable in chunks/delivery but can never form
    // part of the canonical assistant or an action.
    throw new Error('Tool call arguments are not a complete JSON value');
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('Tool call arguments are not a native object');
  }
  return { id: call.id, type: 'function', function: { name, arguments: args } };
}

// Build the exact admitted assistant shape deliberately. Only
// role/content/tool_calls survive; optional SDK properties are dropped when
// empty and rejected when they carry data, so the value corresponds to the
// committed representation instead of whatever the SDK happened to add.
function assistantFrom(message) {
  const source = record(message, 'assistant continuation');
  for (const key of Object.keys(source)) {
    if (!ASSISTANT_MEMBERS.has(key)) {
      throw new Error(`Assistant carries a field outside the carrier: ${key}`);
    }
  }
  for (const extra of SDK_MESSAGE_EXTRAS) {
    const value = source[extra];
    if (value !== undefined && value !== null && value !== '' && !(Array.isArray(value) && value.length === 0)) {
      throw new Error(`Assistant ${extra} is outside the negotiated carrier`);
    }
  }
  if (source.role !== 'assistant' || typeof source.content !== 'string') {
    throw new Error('Incomplete assistant continuation');
  }
  const assistant = { role: 'assistant', content: source.content };
  if (source.tool_calls !== undefined && source.tool_calls !== null) {
    if (!Array.isArray(source.tool_calls) || source.tool_calls.length > MAX_CALLS) {
      throw new Error('Incomplete assistant tool calls');
    }
    const calls = source.tool_calls.map(toolCallFrom);
    if (calls.length) assistant.tool_calls = calls;
  }
  return assistant;
}

// Validate the committed native terminal record, preserving the
// matched-sequence member exactly: string, explicit null, or absent.
function nativeTerminalFrom(raw) {
  if (raw === undefined || raw === null) return undefined;
  const terminal = record(raw, 'native terminal record in continuation delivery');
  if (typeof terminal.stop_reason !== 'string' || typeof terminal.finish_reason !== 'string') {
    throw new Error('Invalid native terminal record in continuation delivery');
  }
  if ('stop_sequence' in terminal && terminal.stop_sequence !== null && typeof terminal.stop_sequence !== 'string') {
    throw new Error('Invalid native stop sequence in continuation delivery');
  }
  const result = { stop_reason: terminal.stop_reason };
  if ('stop_sequence' in terminal) result.stop_sequence = terminal.stop_sequence;
  result.finish_reason = terminal.finish_reason;
  return result;
}

// Validate the committed explicit actionability claim. The carrier emits
// {"tool_calls": [ordered ids]} on every ready delivery it commits under this
// contract revision. An absent member — or the recovery endpoint's literal
// "unavailable" marker for older committed deliveries — produces undefined:
// the turn stays observable and recoverable but exposes no action. Unknown
// claim members are incompatible and rejected.
function actionsFrom(raw) {
  if (raw === undefined || raw === null || raw === 'unavailable') return undefined;
  const actions = record(raw, 'continuation actions in delivery');
  for (const key of Object.keys(actions)) {
    if (!ACTION_MEMBERS.has(key)) throw new Error('Unknown continuation action');
  }
  const claimed = actions.tool_calls;
  if (
    !Array.isArray(claimed) ||
    claimed.length > MAX_CALLS ||
    claimed.some((id) => typeof id !== 'string' || !id)
  ) {
    throw new Error('Invalid continuation actions in delivery');
  }
  return { tool_calls: [...claimed] };
}

// The committed claim must name exactly the ordered assistant calls. A claim
// that adds, drops or reorders identities is an incomplete correspondence and
// the whole delivery is rejected before any action is exposed.
function checkActionCorrespondence(actions, calls) {
  if (actions === undefined) return;
  if (actions.tool_calls.length !== calls.length || actions.tool_calls.some((id, i) => id !== calls[i]?.id)) {
    throw new Error('Continuation actions do not correspond to the assistant tool calls');
  }
}

// The committed record's compatible finish reason must equal the delivered
// finish reason; a contradiction is a corrupt delivery.
function checkTerminal(terminal, finish) {
  if (terminal !== undefined && terminal.finish_reason !== finish) {
    throw new Error('Native terminal record does not match the delivered finish');
  }
}

// The one completed-turn value owned by this helper protocol. Unary,
// streaming and recovered delivery all construct it identically.
function completed({ submission, handle, assistant, observations, finish, usage, nativeUsage, nativeTerminal, actions }) {
  return {
    version: CONTINUATION_VERSION,
    submission,
    handle,
    assistant,
    observations,
    tools: assistant.tool_calls ? [...assistant.tool_calls] : [],
    finish,
    usage,
    nativeUsage,
    nativeTerminal,
    actions
  };
}

// Fold ordered Chat chunk objects into the one completed-turn value. Used by
// live streaming delivery (SDK chunks) and by recovery (the committed
// recorded frames) so both construct exactly the same value.
function assemble(frames, submission) {
  const observations = [];
  const calls = new Map();
  let text = '';
  let readyHandle;
  let finish;
  let usage;
  let nativeUsage;
  let nativeTerminal;
  let actions;
  let terminal = false;
  for (const chunk of frames) {
    const ext = record(chunk, 'continuation delivery frame').olp;
    checkExtension(ext);
    const choices = chunk.choices;
    if (!Array.isArray(choices) || choices.length > 1) {
      throw new Error('Unsupported continuation candidate count');
    }
    if (ext.observation !== undefined) {
      if (terminal) throw new Error('Observation followed terminal delivery');
      observations.push(ext.observation);
    }
    const choice = choices[0];
    if (choice !== undefined) record(choice, 'continuation delivery choice');
    if (choice) {
      const delta = choice.delta === undefined || choice.delta === null ? {} : record(choice.delta, 'continuation delivery delta');
      if (typeof delta.content === 'string') text += delta.content;
      for (const call of delta.tool_calls ?? []) {
        const item = record(call, 'continuation tool delta');
        if (!Number.isSafeInteger(item.index) || item.index < 0) throw new Error('Invalid tool index');
        const existing = calls.get(item.index) ?? { id: '', type: 'function', function: { name: '', arguments: '' } };
        if (item.id) existing.id += item.id;
        if (item.type && item.type !== 'function') throw new Error('Unsupported tool type');
        if (item.function) {
          const fn = record(item.function, 'continuation tool delta');
          if (typeof fn.name === 'string') existing.function.name += fn.name;
          if (typeof fn.arguments === 'string') existing.function.arguments += fn.arguments;
        }
        calls.set(item.index, existing);
      }
      if (choice.finish_reason !== null && choice.finish_reason !== undefined) {
        if (terminal || ext.ready !== true || typeof ext.handle !== 'string') {
          throw new Error('Terminal delivery has no committed continuation');
        }
        finish = requiredString(choice.finish_reason, 'continuation finish reason');
        readyHandle = ext.handle;
        usage = chunk.usage;
        nativeUsage = ext.native_usage;
        nativeTerminal = nativeTerminalFrom(ext.native_terminal);
        checkTerminal(nativeTerminal, finish);
        actions = actionsFrom(ext.actions);
        terminal = true;
      }
    }
  }
  if (!terminal || !HANDLE_PATTERN.test(readyHandle) || !nativeUsage) {
    throw new Error('Incomplete continuation delivery');
  }
  const ordered = [...calls].sort(([a], [b]) => a - b).map(([index, call], position) => {
    if (index !== position || !call.id || !call.function.name || !call.function.arguments) {
      throw new Error('Incomplete or reordered tool call');
    }
    // Streamed calls assemble only from complete argument strings; the same
    // complete-object validation as unary applies before a call may
    // correspond to an action.
    return toolCallFrom(call);
  });
  if ((finish === 'tool_calls') !== (ordered.length > 0)) throw new Error('Tool terminal mismatch');
  checkActionCorrespondence(actions, ordered);
  const assistant = { role: 'assistant', content: text };
  if (ordered.length) assistant.tool_calls = ordered;
  return completed({ submission, handle: readyHandle, assistant, observations, finish, usage, nativeUsage, nativeTerminal, actions });
}

// Fold one Chat completion body — live SDK response or committed recorded
// body — into the same completed-turn value a streamed turn produces.
function completedUnary(body, submission) {
  const source = record(body, 'continuation delivery body');
  const ext = source.olp;
  checkExtension(ext);
  if (ext.ready !== true || typeof ext.handle !== 'string' || ext.native_usage === undefined || ext.native_usage === null) {
    throw new Error('Incomplete continuation delivery');
  }
  const choices = source.choices;
  if (!Array.isArray(choices) || choices.length !== 1) {
    throw new Error('The result has an unsupported candidate count');
  }
  const choice = record(choices[0], 'continuation delivery choice');
  const finish = requiredString(choice.finish_reason, 'continuation finish reason');
  const assistant = assistantFrom(choice.message);
  const observations = ext.observations === undefined ? [] : ext.observations;
  if (!Array.isArray(observations)) throw new Error('Invalid continuation observations');
  const nativeTerminal = nativeTerminalFrom(ext.native_terminal);
  checkTerminal(nativeTerminal, finish);
  const actions = actionsFrom(ext.actions);
  const ordered = assistant.tool_calls ?? [];
  if ((finish === 'tool_calls') !== (ordered.length > 0)) throw new Error('Tool terminal mismatch');
  checkActionCorrespondence(actions, ordered);
  return completed({
    submission,
    handle: ext.handle,
    assistant,
    observations,
    finish,
    usage: source.usage,
    nativeUsage: ext.native_usage,
    nativeTerminal,
    actions
  });
}

// This helper consumes the actual official OpenAI SDK stream. It returns no
// actionable call until the terminal ready marker and all prior observations
// have arrived. Reuse submission across transport retries of the same request.
export async function streamTurn(client, request, { submission = submissionID(), handle } = {}) {
  checkSDK(client);
  const stream = await client.chat.completions.create(
    { ...request, stream: true },
    { headers: continuationHeaders(submission, handle) }
  );
  const chunks = [];
  for await (const chunk of stream) {
    checkExtension(chunk?.olp);
    chunks.push(chunk);
  }
  const value = assemble(chunks, submission);
  value.chunks = chunks;
  return value;
}

// Build the next request for a completed turn. The completed value must be
// the one this helper produced: the negotiated carrier version, a valid
// committed handle and an explicit actions claim that corresponds exactly to
// the assistant's ordered tool calls. A ready handle without a tool action —
// a partial or non-tool outcome, or a delivery recorded before the claim
// existed — can be inspected and recovered but never yields executable calls
// here. The helper never runs tools or claims exactly-once execution.
export function nextTurn(request, completed, results) {
  if (!completed || typeof completed !== 'object' || completed.version !== CONTINUATION_VERSION) {
    throw new TypeError('Provide the completed turn value produced by this helper');
  }
  if (!HANDLE_PATTERN.test(completed.handle)) {
    throw new TypeError('The completed turn has no committed continuation handle');
  }
  const assistant = assistantFrom(completed.assistant);
  const ordered = assistant.tool_calls ?? [];
  const actions = actionsFrom(completed.actions);
  if (actions === undefined) {
    throw new Error('The completed turn has no actionability claim');
  }
  checkActionCorrespondence(actions, ordered);
  const calls = actions.tool_calls;
  if (!calls.length) {
    throw new Error('The completed turn exposes no tool actions');
  }
  if (!Array.isArray(results) || results.length !== calls.length) {
    throw new TypeError('Provide one result for each tool call');
  }
  const tools = results.map((result, index) => {
    if (!result || result.tool_call_id !== ordered[index].id || typeof result.content !== 'string') {
      throw new TypeError('Tool results must match call order and identity');
    }
    return { role: 'tool', tool_call_id: result.tool_call_id, content: result.content };
  });
  const next = { ...request, messages: [...request.messages, assistant, ...tools] };
  delete next.stream;
  delete next.stream_options;
  return next;
}

export async function unaryTurn(client, request, { submission = submissionID(), handle } = {}) {
  checkSDK(client);
  const response = await client.chat.completions.create(
    request,
    { headers: continuationHeaders(submission, handle) }
  );
  checkExtension(response?.olp);
  if (response.olp.ready !== true || !response.olp.handle || !response.olp.native_usage || !response.choices?.[0]?.message) {
    throw new Error('Incomplete continuation delivery');
  }
  const value = completedUnary(response, submission);
  value.response = response;
  return value;
}

// Recovery reads only the committed delivery. It never starts an inference
// request or changes the submission identity after an ambiguous client loss.
// The recorded unary body or streaming frames are reassembled through the
// same construction as a live turn and cross-checked against the committed
// assistant, handle, native terminal record and action claim; a delivery
// committed before the outcome contract existed reads those members as
// "unavailable" and exposes no tool action on replay.
export async function recoverSubmission(origin, apiKey, submission, fetcher = globalThis.fetch) {
  continuationHeaders(submission);
  const address = new URL(`/v1/continuation-submissions/${encodeURIComponent(submission)}`, origin);
  const response = await fetcher(address, {
    headers: { Authorization: `Bearer ${apiKey}`, 'X-OLP-Continuation': CONTINUATION_VERSION }
  });
  const state = await response.json();
  if (!response.ok) {
    const error = new Error(state?.error?.message ?? 'Continuation outcome is unavailable');
    error.status = response.status;
    error.code = state?.error?.code;
    throw error;
  }
  if (state?.version !== CONTINUATION_VERSION || state?.state !== 'ready' ||
      !HANDLE_PATTERN.test(state.handle) || !state.assistant ||
      !state.delivery || typeof state.delivery !== 'object') {
    throw new Error('Incomplete recoverable continuation delivery');
  }
  const delivery = state.delivery;
  let value;
  if (delivery.stream === true && Array.isArray(delivery.frames)) {
    value = assemble(delivery.frames, submission);
  } else if (delivery.stream === false && delivery.body !== undefined && delivery.body !== null) {
    value = completedUnary(delivery.body, submission);
  } else {
    throw new Error('This submission has another delivery shape');
  }
  if (JSON.stringify(assistantFrom(state.assistant)) !== JSON.stringify(value.assistant)) {
    throw new Error('Recovered assistant does not match the committed delivery');
  }
  if (value.handle !== state.handle) {
    throw new Error('Recovered handle does not match the committed delivery');
  }
  if (state.native_terminal === 'unavailable') {
    if (value.nativeTerminal !== undefined) {
      throw new Error('Recovered terminal does not match the committed delivery');
    }
  } else if (state.native_terminal !== undefined &&
             JSON.stringify(nativeTerminalFrom(state.native_terminal)) !== JSON.stringify(value.nativeTerminal)) {
    throw new Error('Recovered terminal does not match the committed delivery');
  }
  if (state.actions === 'unavailable') {
    if (value.actions !== undefined) {
      throw new Error('Recovered actions do not match the committed delivery');
    }
  } else if (state.actions !== undefined &&
             JSON.stringify(actionsFrom(state.actions)) !== JSON.stringify(value.actions)) {
    throw new Error('Recovered actions do not match the committed delivery');
  }
  value.delivery = delivery;
  return value;
}
