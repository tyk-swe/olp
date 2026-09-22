import { randomUUID } from 'node:crypto';

export const CONTINUATION_VERSION = 'chat-anthropic-tools-v1';

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
    if (!/^continuation_[0-9a-f]{32}$/.test(handle)) throw new TypeError('Invalid continuation handle');
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

// This helper consumes the actual official OpenAI SDK stream. It returns no
// actionable call until the terminal ready marker and all prior observations
// have arrived. Reuse submission across transport retries of the same request.
export async function streamTurn(client, request, { submission = submissionID(), handle } = {}) {
  checkSDK(client);
  const stream = await client.chat.completions.create(
    { ...request, stream: true },
    { headers: continuationHeaders(submission, handle) }
  );
  const observations = [];
  const calls = new Map();
  const chunks = [];
  let text = '';
  let readyHandle;
  let finish;
  let usage;
  let terminal = false;
  for await (const chunk of stream) {
    checkExtension(chunk.olp);
    chunks.push(chunk);
    const choice = chunk.choices?.[0];
    if (chunk.olp.observation) {
      if (terminal) throw new Error('Observation followed terminal delivery');
      observations.push(chunk.olp.observation);
    }
    if (choice?.delta?.content) text += choice.delta.content;
    for (const call of choice?.delta?.tool_calls ?? []) {
      if (!Number.isSafeInteger(call.index) || call.index < 0) throw new Error('Invalid tool index');
      const existing = calls.get(call.index) ?? { id: '', type: 'function', function: { name: '', arguments: '' } };
      if (call.id) existing.id += call.id;
      if (call.type && call.type !== 'function') throw new Error('Unsupported tool type');
      if (call.function?.name) existing.function.name += call.function.name;
      if (call.function?.arguments) existing.function.arguments += call.function.arguments;
      calls.set(call.index, existing);
    }
    if (choice?.finish_reason) {
      if (terminal || chunk.olp.ready !== true || typeof chunk.olp.handle !== 'string') {
        throw new Error('Terminal delivery has no committed continuation');
      }
      finish = choice.finish_reason;
      readyHandle = chunk.olp.handle;
      usage = chunk.usage;
      terminal = true;
    }
  }
  if (!terminal || !/^continuation_[0-9a-f]{32}$/.test(readyHandle)) {
    throw new Error('Incomplete continuation delivery');
  }
  const toolCalls = [...calls].sort(([a], [b]) => a - b).map(([index, call], position) => {
    if (index !== position || !call.id || !call.function.name || !call.function.arguments) {
      throw new Error('Incomplete or reordered tool call');
    }
    return call;
  });
  if ((finish === 'tool_calls') !== (toolCalls.length > 0)) throw new Error('Tool terminal mismatch');
  const assistant = { role: 'assistant', content: text };
  if (toolCalls.length) assistant.tool_calls = toolCalls;
  return { submission, handle: readyHandle, assistant, observations, chunks, finish, usage };
}

export function nextTurn(request, completed, results) {
  if (!completed?.handle || !Array.isArray(completed.assistant?.tool_calls)) {
    throw new TypeError('A ready tool continuation is required');
  }
  if (!Array.isArray(results) || results.length !== completed.assistant.tool_calls.length) {
    throw new TypeError('Provide one result for each tool call');
  }
  const tools = results.map((result, index) => {
    if (result?.tool_call_id !== completed.assistant.tool_calls[index].id || typeof result.content !== 'string') {
      throw new TypeError('Tool results must match call order and identity');
    }
    return { role: 'tool', tool_call_id: result.tool_call_id, content: result.content };
  });
  const next = { ...request, messages: [...request.messages, completed.assistant, ...tools] };
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
  checkExtension(response.olp);
  if (response.olp.ready !== true || !response.olp.handle || !response.choices?.[0]?.message) {
    throw new Error('Incomplete continuation delivery');
  }
  return { submission, handle: response.olp.handle, assistant: response.choices[0].message, response };
}

// Recovery reads only the committed delivery. It never starts an inference
// request or changes the submission identity after an ambiguous client loss.
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
      !/^continuation_[0-9a-f]{32}$/.test(state.handle) || !state.assistant || !state.delivery) {
    throw new Error('Incomplete recoverable continuation delivery');
  }
  return state;
}
