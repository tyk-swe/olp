import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4190;
const origin = `http://${host}:${port}`;
const model = 'fixture-model';
const secret = 'ocr-search-fixture-secret';
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

const paths = ['/mistral/v1/ocr', '/perplexity/search', '/tavily/search'];

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
  if (request.method === 'GET' && path.endsWith('/models'))
    return answer(response, 200, JSON.stringify({ data: [{ id: model }] }));
  if (request.method !== 'POST' || !paths.includes(path)) {
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
    return answer(response, 401, '{"detail":[{"msg":"incorrect credential"}]}');
  }
  if (path === '/mistral/v1/ocr') {
    if (body.model !== model) {
      unexpected.push('incorrect serving model');
      return answer(response, 404, '{"error":{"message":"incorrect model"}}');
    }
    return answer(
      response,
      200,
      JSON.stringify({
        pages: [
          {
            index: 0,
            markdown: '# Fixture document\n\nFixture OCR text.',
            images: [],
            dimensions: { dpi: 200, height: 2200, width: 1700 }
          }
        ],
        model,
        document_annotation: null,
        usage_info: { pages_processed: 1, doc_size_bytes: 1048576 }
      })
    );
  }
  if (path === '/perplexity/search') {
    if (typeof body.query !== 'string') {
      unexpected.push('invalid perplexity query');
      return answer(
        response,
        422,
        '{"detail":[{"loc":["body","query"],"msg":"field required","type":"missing"}]}'
      );
    }
    return answer(
      response,
      200,
      JSON.stringify({
        id: 'ppx_fx_1',
        results: [
          {
            title: 'Fixture result',
            url: 'https://fixture.example/result',
            snippet: 'Fixture snippet.',
            date: '2026-10-01',
            last_updated: '2026-10-15'
          }
        ],
        server_time: '2026-10-19T07:00:00Z'
      })
    );
  }
  if (path === '/tavily/search') {
    if (typeof body.query !== 'string') {
      unexpected.push('invalid tavily query');
      return answer(
        response,
        422,
        '{"detail":[{"loc":["body","query"],"msg":"field required","type":"missing"}]}'
      );
    }
    return answer(
      response,
      200,
      JSON.stringify({
        query: body.query,
        results: [
          {
            title: 'Fixture result',
            url: 'https://fixture.example/result',
            content: 'Fixture content.',
            score: 0.91
          }
        ],
        response_time: 1.2,
        request_id: 'tvly_fx_1'
      })
    );
  }
});

server.listen(port, host, () => {
  console.log(`mock ocr/search upstream on ${origin}`);
});
