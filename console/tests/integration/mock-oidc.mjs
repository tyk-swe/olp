import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4176;
const issuer = `http://${host}:${port}`;

const discovery = {
  issuer,
  authorization_endpoint: `${issuer}/authorize`,
  token_endpoint: `${issuer}/token`,
  jwks_uri: `${issuer}/jwks`,
  response_types_supported: ['code'],
  code_challenge_methods_supported: ['S256'],
  token_endpoint_auth_methods_supported: ['client_secret_basic'],
  id_token_signing_alg_values_supported: ['EdDSA']
};

const jwks = {
  keys: [
    {
      kty: 'OKP',
      crv: 'Ed25519',
      x: 'WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc',
      kid: 'test-key',
      alg: 'EdDSA',
      use: 'sig'
    }
  ]
};

function json(response, value) {
  const body = JSON.stringify(value);
  response.writeHead(200, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(body),
    'cache-control': 'no-store'
  });
  response.end(body);
}

createServer((request, response) => {
  const path = new URL(request.url ?? '/', issuer).pathname;
  if (path === '/.well-known/openid-configuration') return json(response, discovery);
  if (path === '/jwks') return json(response, jwks);
  if (path === '/authorize') {
    response.writeHead(200, { 'content-type': 'text/plain', 'cache-control': 'no-store' });
    response.end('Mock identity provider authorization boundary');
    return;
  }
  response.writeHead(404, { 'content-type': 'text/plain' });
  response.end('not found');
}).listen(port, host);
