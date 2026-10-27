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
const bravePath = '/brave/res/v1/web/search';
const azureSubmit = `/azure/documentintelligence/documentModels/${model}:analyze`;
const azurePoll = `/azure/documentintelligence/documentModels/${model}/analyzeResults/fixture-result`;

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
  if (path === azurePoll && request.method === 'DELETE') {
    calls.push({ path, raw: '', body: null, headers: request.headers });
    response.writeHead(204, { 'cache-control': 'no-store' });
    return response.end();
  }
  if (path === azurePoll && request.method === 'GET') {
    calls.push({ path, raw: '', body: null, headers: request.headers });
    if (request.headers['ocp-apim-subscription-key'] !== secret) {
      unexpected.push('incorrect azure credential');
      return answer(response, 401, '{"error":{"code":"401"}}');
    }
    const content = '# Azure fixture\n\nAzure fixture OCR text.';
    return answer(
      response,
      200,
      JSON.stringify({
        status: 'succeeded',
        analyzeResult: {
          apiVersion: '2024-11-30',
          modelId: model,
          stringIndexType: 'utf16CodeUnit',
          contentFormat: 'markdown',
          content,
          pages: [
            {
              pageNumber: 1,
              spans: [{ offset: 0, length: content.length }]
            }
          ]
        }
      })
    );
  }
  if (request.method === 'POST' && path === azureSubmit) {
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
    if (request.headers['ocp-apim-subscription-key'] !== secret) {
      unexpected.push('incorrect azure credential');
      return answer(response, 401, '{"error":{"code":"401"}}');
    }
    if (body.urlSource === undefined && body.base64Source === undefined) {
      unexpected.push('invalid azure body');
      return answer(response, 400, '{"error":{"code":"InvalidRequest"}}');
    }
    response.writeHead(202, {
      'operation-location': `${origin}${azurePoll}?api-version=2024-11-30`,
      'retry-after': '0',
      'cache-control': 'no-store'
    });
    return response.end();
  }
  if (path === bravePath) {
    const query = new URL(request.url ?? '/', origin).searchParams;
    calls.push({
      path,
      method: request.method,
      query: Object.fromEntries(query),
      raw: '',
      body: null,
      headers: request.headers
    });
    if (request.method !== 'GET') {
      unexpected.push('brave called without GET');
      return answer(response, 405, '{"error":{"message":"method"}}');
    }
    if (request.headers['x-subscription-token'] !== secret) {
      unexpected.push('incorrect provider credential');
      return answer(
        response,
        401,
        '{"type":"ErrorResponse","error":{"detail":"incorrect credential"}}'
      );
    }
    if (!query.get('q')) {
      unexpected.push('invalid brave query');
      return answer(
        response,
        422,
        '{"type":"ErrorResponse","error":{"detail":"Missing query parameter: q"}}'
      );
    }
    return answer(
      response,
      200,
      JSON.stringify({
        type: 'search',
        query: { original: query.get('q'), more_results_available: false },
        web: {
          type: 'search',
          results: [
            {
              title: 'Fixture result',
              url: 'https://fixture.example/result',
              description: 'Fixture description.',
              page_age: '2026-10-01T00:00:00Z',
              family_friendly: true,
              type: 'search_result'
            }
          ],
          family_friendly: true
        }
      })
    );
  }
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
  calls.push({
    path,
    method: request.method,
    query: {},
    raw,
    body,
    headers: request.headers
  });
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
