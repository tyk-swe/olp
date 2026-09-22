import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4188;
const origin = `http://${host}:${port}`;
const model = 'fixture-model';
const secret = 'anthropic-browser-fixture-secret';
const stream = readFileSync(
  new URL(
    '../../../tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse',
    import.meta.url
  ),
  'utf8'
);
const expectedNext = JSON.parse(
  readFileSync(
    new URL(
      '../../../tests/fixtures/fidelity/v1/anthropic-tool-next-request.json',
      import.meta.url
    ),
    'utf8'
  )
);
const calls = [];
const unexpected = [];

function json(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    'content-type': 'application/json',
    'cache-control': 'no-store'
  });
  response.end(body);
}

function message(text) {
  return {
    id: 'msg-browser-fixture',
    type: 'message',
    role: 'assistant',
    model,
    content: [{ type: 'text', text }],
    stop_reason: 'end_turn',
    stop_sequence: null,
    usage: { input_tokens: 30, output_tokens: 4 }
  };
}

function simpleStream(response) {
  const frames = [
    [
      'message_start',
      {
        type: 'message_start',
        message: { ...message(''), content: [], stop_reason: null }
      }
    ],
    [
      'content_block_start',
      {
        type: 'content_block_start',
        index: 0,
        content_block: { type: 'text', text: '' }
      }
    ],
    [
      'content_block_delta',
      {
        type: 'content_block_delta',
        index: 0,
        delta: { type: 'text_delta', text: 'OK' }
      }
    ],
    ['content_block_stop', { type: 'content_block_stop', index: 0 }],
    [
      'message_delta',
      {
        type: 'message_delta',
        delta: { stop_reason: 'end_turn', stop_sequence: null },
        usage: { output_tokens: 1 }
      }
    ],
    ['message_stop', { type: 'message_stop' }]
  ];
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-store'
  });
  for (const [name, value] of frames)
    response.write(`event: ${name}\ndata: ${JSON.stringify(value)}\n\n`);
  response.end();
}

async function body(request) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > 1 << 20) throw new Error('body too large');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString('utf8');
}

const server = createServer(async (request, response) => {
  const path = new URL(request.url ?? '/', origin).pathname;
  if (request.method === 'GET' && path === '/health')
    return json(response, 200, { status: 'ok' });
  if (request.method === 'POST' && path === '/__test__/reset') {
    calls.length = 0;
    unexpected.length = 0;
    response.writeHead(204, { 'cache-control': 'no-store' });
    return response.end();
  }
  if (request.method === 'GET' && path === '/__test__/requests')
    return json(response, 200, { calls, unexpected });
  if (request.method === 'GET' && path === '/v1/models')
    return json(response, 200, { data: [{ id: model }] });
  if (request.method !== 'POST' || path !== '/v1/messages') {
    unexpected.push(`${request.method} ${path}`);
    return json(response, 404, {
      error: { type: 'not_found_error', message: 'unexpected route' }
    });
  }
  let raw;
  let parsed;
  try {
    raw = await body(request);
    parsed = JSON.parse(raw);
  } catch {
    return json(response, 400, {
      error: { type: 'invalid_request_error', message: 'invalid body' }
    });
  }
  calls.push({ path, raw, body: parsed, headers: request.headers });
  if (
    request.headers['x-api-key'] !== secret &&
    request.headers.authorization !== `Bearer ${secret}`
  ) {
    unexpected.push('incorrect provider credential');
    return json(response, 401, {
      error: { type: 'authentication_error', message: 'invalid credential' }
    });
  }
  if (parsed.model !== model) {
    unexpected.push('incorrect serving model');
    return json(response, 404, {
      error: { type: 'not_found_error', message: 'incorrect model' }
    });
  }
  if (
    parsed.tools?.length === 2 &&
    parsed.messages?.length === 1 &&
    parsed.stream === true
  ) {
    response.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-store'
    });
    return response.end(stream);
  }
  if (parsed.tools?.length === 2 && parsed.messages?.length === 3) {
    try {
      assert.deepEqual(parsed, expectedNext);
    } catch {
      unexpected.push('native next request changed');
      return json(response, 400, {
        error: {
          type: 'invalid_request_error',
          message: 'native continuation changed'
        }
      });
    }
    return json(response, 200, message('Both tools completed.'));
  }
  if (parsed.stream === true) return simpleStream(response);
  return json(response, 200, message('OK'));
});

server.listen(port, host);
for (const signal of ['SIGINT', 'SIGTERM'])
  process.on(signal, () => server.close(() => process.exit(0)));
