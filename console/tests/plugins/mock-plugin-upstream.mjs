import { createHash, randomBytes } from 'node:crypto';
import { createServer } from 'node:http';

// The upstream the reference plugin's profiles place requests at when a
// journey links it against this server. It speaks OpenAI Chat Completions but
// authenticates the way the reference plugin declares: a token in the
// Authorization header, or an access token its authority issued with the
// account it authorizes, and a client header. It records every request so
// the journey can prove the hosting adaptation placed them.
//
// Under /oauth it is also the reference plugin's authority, a fake OAuth 2.0
// server running the authorization code flow with PKCE (S256): /authorize
// signs the operator in at once and redirects to the loopback callback with a
// code, /token exchanges each code once for the verifier that matches its
// challenge, and /userinfo names the account's subject.
const host = '127.0.0.1';
const port = 4190;
const origin = `http://${host}:${port}`;
const model = 'reference-e2e-model';
const credential = 'reference-plugin-secret';
const identity = { subject: 'operator@reference.example', account: 'acct-e2e' };
const reply = 'Hello from the plugin upstream';
const maxBodyBytes = 1 << 20;
const usage = { prompt_tokens: 7, completion_tokens: 5, total_tokens: 12 };

const recorded = [];
const unexpected = [];
const codes = new Map();
const accessTokens = new Set();

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

async function readText(request) {
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
  return Buffer.concat(chunks).toString('utf8');
}

async function readJson(request) {
  const body = await readText(request);
  return body ? JSON.parse(body) : null;
}

function oauthError(response, status, code, description) {
  return json(response, status, {
    error: code,
    error_description: description
  });
}

function secret() {
  return randomBytes(24).toString('base64url');
}

async function authority(request, response, url) {
  const method = request.method ?? 'GET';
  if (method === 'GET' && url.pathname === '/oauth/authorize') {
    const query = url.searchParams;
    const redirect = query.get('redirect_uri');
    if (
      !redirect ||
      query.get('response_type') !== 'code' ||
      !query.get('client_id') ||
      query.get('code_challenge_method') !== 'S256' ||
      !query.get('code_challenge')
    )
      return oauthError(response, 400, 'invalid_request', 'bad request');
    const code = secret();
    codes.set(code, {
      clientId: query.get('client_id'),
      redirect,
      challenge: query.get('code_challenge')
    });
    const callback = new URL(redirect);
    callback.searchParams.set('code', code);
    callback.searchParams.set('state', query.get('state') ?? '');
    response.writeHead(302, {
      location: callback.toString(),
      'cache-control': 'no-store'
    });
    response.end();
    return;
  }
  if (method === 'POST' && url.pathname === '/oauth/token') {
    const form = new URLSearchParams(await readText(request));
    const code = codes.get(form.get('code') ?? '');
    codes.delete(form.get('code') ?? '');
    const verified = createHash('sha256')
      .update(form.get('code_verifier') ?? '')
      .digest('base64url');
    if (
      form.get('grant_type') !== 'authorization_code' ||
      !code ||
      code.clientId !== form.get('client_id') ||
      code.redirect !== form.get('redirect_uri') ||
      code.challenge !== verified
    )
      return oauthError(response, 400, 'invalid_grant', 'code refused');
    const accessToken = secret();
    accessTokens.add(accessToken);
    return json(response, 200, {
      access_token: accessToken,
      refresh_token: secret(),
      token_type: 'Bearer',
      expires_in: 3600,
      account: identity.account
    });
  }
  if (method === 'GET' && url.pathname === '/oauth/userinfo') {
    const token = (request.headers.authorization ?? '').replace(/^Bearer /, '');
    if (!accessTokens.has(token))
      return oauthError(response, 401, 'invalid_token', 'unknown token');
    return json(response, 200, { sub: identity.subject });
  }
  return oauthError(response, 404, 'not_found', 'unknown endpoint');
}

function authorized(headers) {
  const bearer = (headers.authorization ?? '').replace(/^Bearer /, '');
  return (
    headers['x-reference-client'] === 'olp' &&
    (headers.authorization === `Token ${credential}` ||
      (accessTokens.has(bearer) &&
        headers['x-reference-account'] === identity.account))
  );
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
  if (url.pathname.startsWith('/oauth/'))
    return authority(request, response, url);
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
    codes.clear();
    accessTokens.clear();
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
  if (!authorized(request.headers)) {
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
