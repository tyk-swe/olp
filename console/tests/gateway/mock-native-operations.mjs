import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4189;
const origin = `http://${host}:${port}`;
const model = 'fixture-model';
const secret = 'native-browser-fixture-secret';
const calls = [];
const unexpected = [];

function answer(response, status, body) {
  response.writeHead(status, {
    'content-type': 'application/json',
    'cache-control': 'no-store'
  });
  response.end(body);
}

async function read(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > 1 << 20) throw new Error('body limit');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString('utf8');
}

const server = createServer(async (request, response) => {
  const path = new URL(request.url ?? '/', origin).pathname;
  if (request.method === 'GET' && path === '/health')
    return answer(response, 200, '{"status":"ok"}');
  if (request.method === 'POST' && path === '/__test__/reset') {
    calls.length = 0;
    unexpected.length = 0;
    response.writeHead(204, { 'cache-control': 'no-store' });
    return response.end();
  }
  if (request.method === 'GET' && path === '/__test__/requests')
    return answer(response, 200, JSON.stringify({ calls, unexpected }));
  if (request.method === 'GET' && path === '/v1/models')
    return answer(response, 200, JSON.stringify({ data: [{ id: model }] }));
  if (
    request.method !== 'POST' ||
    !['/v1/embeddings', '/v1/rerank'].includes(path)
  ) {
    unexpected.push(`${request.method} ${path}`);
    return answer(
      response,
      404,
      '{"error":{"message":"unregistered fixture path"}}'
    );
  }
  let raw;
  let body;
  try {
    raw = await read(request);
    body = JSON.parse(raw);
  } catch {
    return answer(
      response,
      400,
      '{"error":{"message":"invalid fixture body"}}'
    );
  }
  calls.push({ path, raw, body, headers: request.headers });
  if (request.headers.authorization !== `Bearer ${secret}`) {
    unexpected.push('incorrect provider credential');
    return answer(
      response,
      401,
      '{"error":{"message":"incorrect provider credential"}}'
    );
  }
  if (path === '/v1/embeddings') {
    if (body.model !== model) {
      unexpected.push('incorrect serving model');
      return answer(response, 404, '{"error":{"message":"incorrect model"}}');
    }
    if (body.output_dtype === 'ubinary' && body.output_dimension === 16) {
      if (!Array.isArray(body.input) || body.input.length !== 2) {
        unexpected.push('packed input ordering changed');
        return answer(
          response,
          400,
          '{"error":{"message":"packed input changed"}}'
        );
      }
      return answer(
        response,
        200,
        '{"object":"list","data":[{"object":"embedding","index":0,"embedding":"AP8="},{"object":"embedding","index":1,"embedding":"AQI="}],"model":"fixture-model","usage":{"total_tokens":2},"native":{"unsafe":9007199254740993,"negative_zero":-0}}'
      );
    }
    return answer(
      response,
      200,
      '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,-0.2]}],"model":"fixture-model","usage":{"total_tokens":2}}'
    );
  }
  if (!Array.isArray(body.texts) || body.texts.length !== 2) {
    unexpected.push('rerank candidate set changed');
    return answer(
      response,
      400,
      '{"error":{"message":"candidate set changed"}}'
    );
  }
  return answer(
    response,
    200,
    '[{"index":1,"score":0.1000000000000000000001},{"index":0,"score":0.1000000000000000000001}]'
  );
});

server.listen(port, host);
for (const signal of ['SIGINT', 'SIGTERM'])
  process.on(signal, () => server.close(() => process.exit(0)));
