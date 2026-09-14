import { createServer } from 'node:http';

// An OpenAI-compatible upstream for the Go gateway journeys. It records every
// request so the journey can prove which credential reached it and that the
// client API key never did.
const host = '127.0.0.1';
const port = 4187;
const origin = `http://${host}:${port}`;
const model = 'compatible-e2e-model';
const credential = 'compatible-provider-secret';
const reply = 'Hello from the compatible upstream';
const inputTokens = 7;
const outputTokens = 5;
const maxBodyBytes = 1 << 20;

const recorded = [];
const unexpected = [];
let chunkDelayMs = 25;
let holdStreams = false;
const waitingStreams = new Set();

function releaseStreams() {
  holdStreams = false;
  for (const resume of waitingStreams) resume();
}

function json(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(body),
    'cache-control': 'no-store'
  });
  response.end(body);
}

function openaiError(response, status, code, message) {
  return json(response, status, {
    error: { message, type: 'invalid_request_error', param: null, code }
  });
}

async function readJson(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > maxBodyBytes) {
      const error = new Error('request body exceeded the journey limit');
      error.status = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  const body = Buffer.concat(chunks).toString('utf8');
  return body ? JSON.parse(body) : null;
}

const usage = {
  prompt_tokens: inputTokens,
  completion_tokens: outputTokens,
  total_tokens: inputTokens + outputTokens
};

function chatResponse() {
  return {
    id: 'chatcmpl-compatible-e2e',
    object: 'chat.completion',
    created: 1,
    model,
    choices: [
      {
        index: 0,
        message: { role: 'assistant', content: reply },
        finish_reason: 'stop'
      }
    ],
    usage
  };
}

function responsesResponse() {
  return {
    id: 'resp_compatible_e2e',
    object: 'response',
    created_at: 1,
    status: 'completed',
    model,
    output: [
      {
        id: 'msg_compatible_e2e',
        type: 'message',
        role: 'assistant',
        status: 'completed',
        content: [{ type: 'output_text', text: reply, annotations: [] }]
      }
    ],
    usage: {
      input_tokens: inputTokens,
      output_tokens: outputTokens,
      total_tokens: inputTokens + outputTokens
    }
  };
}

function chunk(delta, finish, withUsage) {
  const value = {
    id: 'chatcmpl-compatible-stream',
    object: 'chat.completion.chunk',
    created: 1,
    model,
    choices: [{ index: 0, delta, finish_reason: finish }]
  };
  if (withUsage) value.usage = usage;
  return `data: ${JSON.stringify(value)}\n\n`;
}

async function streamReply(response, responses) {
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-store'
  });
  const completion = responsesResponse();
  const words = reply.split(' ');
  const events = responses
    ? [
        `event: response.created\ndata: ${JSON.stringify({ type: 'response.created', response: { ...completion, status: 'in_progress', output: [] } })}\n\n`,
        ...words.map(
          (word, index) =>
            `event: response.output_text.delta\ndata: ${JSON.stringify({ type: 'response.output_text.delta', output_index: 0, delta: index === 0 ? word : ` ${word}` })}\n\n`
        ),
        `event: response.completed\ndata: ${JSON.stringify({ type: 'response.completed', response: completion })}\n\n`
      ]
    : [
        chunk({ role: 'assistant', content: '' }, null, false),
        ...words.map((word, index) =>
          chunk({ content: index === 0 ? word : ` ${word}` }, null, false)
        ),
        chunk({}, 'stop', true),
        'data: [DONE]\n\n'
      ];
  for (const [index, event] of events.entries()) {
    if (response.destroyed) return;
    response.write(event);
    if (index === 0 && holdStreams) {
      await new Promise((resolve) => {
        const resume = () => {
          response.off('close', resume);
          waitingStreams.delete(resume);
          resolve();
        };
        waitingStreams.add(resume);
        response.once('close', resume);
        if (response.destroyed) resume();
      });
    }
    await new Promise((resolve) => setTimeout(resolve, chunkDelayMs));
  }
  response.end();
}

const server = createServer(async (request, response) => {
  const method = request.method ?? 'GET';
  const url = new URL(request.url ?? '/', origin);
  if (method === 'GET' && url.pathname === '/health') {
    return json(response, 200, { status: 'ok' });
  }
  let body;
  try {
    body = await readJson(request);
  } catch (error) {
    const status = typeof error?.status === 'number' ? error.status : 400;
    return openaiError(
      response,
      status,
      'invalid_body',
      'invalid request body'
    );
  }
  if (method === 'POST' && url.pathname === '/__test__/reset') {
    releaseStreams();
    recorded.length = 0;
    unexpected.length = 0;
    chunkDelayMs = 25;
    response.writeHead(204, { 'cache-control': 'no-store' });
    response.end();
    return;
  }
  if (method === 'POST' && url.pathname === '/__test__/delay') {
    chunkDelayMs = Number(body?.ms ?? 25);
    response.writeHead(204, { 'cache-control': 'no-store' });
    response.end();
    return;
  }
  if (
    method === 'POST' &&
    ['/__test__/hold', '/__test__/resume'].includes(url.pathname)
  ) {
    if (url.pathname === '/__test__/hold') holdStreams = true;
    else releaseStreams();
    response.writeHead(204, { 'cache-control': 'no-store' });
    response.end();
    return;
  }
  if (method === 'GET' && url.pathname === '/__test__/requests') {
    return json(response, 200, { requests: recorded, unexpected });
  }
  recorded.push({
    method,
    path: url.pathname,
    query: url.searchParams.toString(),
    headers: request.headers,
    body
  });
  if (request.headers.authorization !== `Bearer ${credential}`) {
    unexpected.push(`${method} ${url.pathname}: incorrect credential`);
    return openaiError(
      response,
      401,
      'invalid_api_key',
      'incorrect provider credential'
    );
  }
  if (method === 'GET' && url.pathname === '/v1/models') {
    return json(response, 200, {
      object: 'list',
      data: [{ id: model, object: 'model', created: 1, owned_by: 'mock' }]
    });
  }
  const chat = url.pathname === '/v1/chat/completions';
  const responses = url.pathname === '/v1/responses';
  if (method !== 'POST' || (!chat && !responses)) {
    unexpected.push(`${method} ${url.pathname}`);
    return openaiError(response, 404, 'not_found', 'unexpected upstream path');
  }
  if (!body || typeof body !== 'object' || Array.isArray(body)) {
    unexpected.push(`${method} ${url.pathname}: expected a JSON object`);
    return openaiError(response, 400, 'invalid_body', 'expected a JSON object');
  }
  if (body.model !== model) {
    unexpected.push(`${method} ${url.pathname}: unexpected model`);
    return openaiError(
      response,
      404,
      'model_not_found',
      'the configured upstream model was not requested'
    );
  }
  if (body.stream === true) return streamReply(response, responses);
  return json(response, 200, responses ? responsesResponse() : chatResponse());
});

function shutdown() {
  releaseStreams();
  server.close(() => process.exit(0));
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
server.listen(port, host);
