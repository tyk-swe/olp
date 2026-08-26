import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4178;
const origin = `http://${host}:${port}`;
const deployment = 'vertical-e2e-deployment';
const apiVersion = '2024-10-21';
const credential = 'vertical-provider-secret';
const reply = 'Hello from the vertical upstream';
const inputTokens = 7;
const outputTokens = 5;
const maxBodyBytes = 1 << 20;

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

async function readJson(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > maxBodyBytes) {
      const error = new Error(
        'request body exceeded the integration-test limit'
      );
      error.status = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  const body = Buffer.concat(chunks).toString('utf8');
  return body ? JSON.parse(body) : null;
}

function chatResponse() {
  return {
    id: 'chatcmpl-vertical-e2e',
    object: 'chat.completion',
    created: 1,
    model: deployment,
    choices: [
      {
        index: 0,
        message: { role: 'assistant', content: reply },
        finish_reason: 'stop'
      }
    ],
    usage: {
      prompt_tokens: inputTokens,
      completion_tokens: outputTokens,
      total_tokens: inputTokens + outputTokens
    }
  };
}

function responsesResponse() {
  return {
    id: 'resp_vertical_e2e',
    object: 'response',
    created_at: 1,
    status: 'completed',
    model: deployment,
    output: [
      {
        id: 'msg_vertical_e2e',
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

const azureChat = `/openai/deployments/${deployment}/chat/completions`;
const azureResponses = `/openai/deployments/${deployment}/responses`;

function rejectPayload(response, item, detail) {
  unexpected.push(`${item.method} ${item.path}: ${detail}`);
  return json(response, 400, { error: detail });
}

function expectedTokenLimit(prompt) {
  if (prompt === 'OLP capability probe') return 1;
  if (prompt === 'Connection test') return 16;
  return null;
}

function validateTokenLimit(prompt, actualLimit, label) {
  const tokenLimit = expectedTokenLimit(prompt);
  if (tokenLimit === null)
    return `${label} payload contained an unexpected prompt`;
  if (actualLimit !== tokenLimit)
    return `${label} payload omitted the translated token limit`;
  return null;
}

function validateResponsesPayload(body) {
  if (
    !Array.isArray(body.input) ||
    body.input.length !== 1 ||
    body.input[0]?.type !== 'message' ||
    body.input[0]?.role !== 'user' ||
    !Array.isArray(body.input[0]?.content) ||
    body.input[0].content.length !== 1 ||
    body.input[0].content[0]?.type !== 'input_text' ||
    typeof body.input[0].content[0]?.text !== 'string'
  ) {
    return 'Responses payload omitted the translated user input';
  }
  return validateTokenLimit(
    body.input[0].content[0].text,
    body.max_output_tokens,
    'Responses'
  );
}

function validateChatPayload(body) {
  if (
    !Array.isArray(body.messages) ||
    body.messages.length !== 1 ||
    body.messages[0]?.role !== 'user' ||
    typeof body.messages[0]?.content !== 'string'
  ) {
    return 'Chat Completions payload omitted the translated user message';
  }
  return validateTokenLimit(
    body.messages[0].content,
    body.max_completion_tokens,
    'Chat Completions'
  );
}

const server = createServer(async (request, response) => {
  const method = request.method ?? 'GET';
  const url = new URL(request.url ?? '/', origin);

  if (method === 'GET' && url.pathname === '/health') {
    return json(response, 200, { status: 'ok' });
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

  let body;
  try {
    body = await readJson(request);
  } catch (error) {
    const status = typeof error?.status === 'number' ? error.status : 400;
    return json(response, status, { error: 'invalid request body' });
  }

  const item = {
    method,
    path: url.pathname,
    query: url.searchParams.toString(),
    headers: request.headers,
    body
  };
  recorded.push(item);

  if (
    method !== 'POST' ||
    (url.pathname !== azureChat && url.pathname !== azureResponses)
  ) {
    unexpected.push(`${method} ${url.pathname}`);
    return json(response, 404, { error: 'unexpected Azure OpenAI path' });
  }
  if (url.searchParams.get('api-version') !== apiVersion) {
    unexpected.push(`${method} ${url.pathname}: incorrect api-version`);
    return json(response, 400, { error: 'incorrect api-version' });
  }
  if (request.headers['api-key'] !== credential) {
    unexpected.push(`${method} ${url.pathname}: incorrect credential`);
    return json(response, 401, { error: 'incorrect provider credential' });
  }
  if (!body || typeof body !== 'object' || Array.isArray(body)) {
    return rejectPayload(response, item, 'expected a JSON object');
  }
  if (body.model !== deployment) {
    return rejectPayload(
      response,
      item,
      'payload omitted the configured deployment model'
    );
  }
  if (body.stream !== undefined && body.stream !== false) {
    return rejectPayload(
      response,
      item,
      'this integration mock supports unary requests only'
    );
  }
  const payloadError =
    url.pathname === azureResponses
      ? validateResponsesPayload(body)
      : validateChatPayload(body);
  if (payloadError) {
    return rejectPayload(response, item, payloadError);
  }

  return json(
    response,
    200,
    url.pathname === azureResponses ? responsesResponse() : chatResponse()
  );
});

function shutdown() {
  server.close(() => process.exit(0));
}

process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
server.listen(port, host);
