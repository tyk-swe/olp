import { createServer } from 'node:http';

// The upstream the reference plugin's profile places requests at when a
// journey links it against this server. It speaks OpenAI Chat Completions but
// authenticates the way the reference plugin declares: a token in the
// Authorization header and a client header. It records every request so the
// journey can prove the hosting adaptation placed both.
const host = '127.0.0.1';
const port = 4190;
const origin = `http://${host}:${port}`;
const model = 'reference-e2e-model';
const credential = 'reference-plugin-secret';
const reply = 'Hello from the plugin upstream';
const maxBodyBytes = 1 << 20;
const usage = { prompt_tokens: 7, completion_tokens: 5, total_tokens: 12 };

const recorded = [];
const unexpected = [];

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

function chunk(delta, finish, withUsage) {
  const value = {
    id: 'chatcmpl-plugin-stream',
    object: 'chat.completion.chunk',
    created: 1,
    model,
    choices: [{ index: 0, delta, finish_reason: finish }]
  };
  if (withUsage) value.usage = usage;
  return `data: ${JSON.stringify(value)}\n\n`;
}

function streamReply(response) {
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-store'
  });
  const words = reply.split(' ');
  response.write(chunk({ role: 'assistant', content: '' }, null, false));
  for (const [index, word] of words.entries())
    response.write(chunk({ content: index === 0 ? word : ` ${word}` }, null));
  response.write(chunk({}, 'stop', true));
  response.end('data: [DONE]\n\n');
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
    return openaiError(response, status, 'invalid_body', 'invalid body');
  }
  if (method === 'POST' && url.pathname === '/__test__/reset') {
    recorded.length = 0;
    unexpected.length = 0;
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
  if (
    request.headers.authorization !== `Token ${credential}` ||
    request.headers['x-reference-client'] !== 'olp'
  ) {
    unexpected.push(`${method} ${url.pathname}: incorrect credential`);
    return openaiError(
      response,
      401,
      'invalid_api_key',
      'incorrect provider credential'
    );
  }
  if (method !== 'POST' || url.pathname !== '/v1/chat/completions') {
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
      'the declared upstream model was not requested'
    );
  }
  if (body.stream === true) return streamReply(response);
  return json(response, 200, {
    id: 'chatcmpl-plugin',
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
  });
});

function shutdown() {
  server.close(() => process.exit(0));
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
server.listen(port, host);
