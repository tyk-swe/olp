import { createServer } from 'node:http';
import { mkdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const directory =
  process.env.OLP_CONSOLE_E2E_VAULT_DIR ??
  join(tmpdir(), 'olp-vault-browser-fixture');
await mkdir(directory, { recursive: true, mode: 0o700 });
await writeFile(join(directory, 'identity.jwt'), 'fixture.browser.jwt', {
  mode: 0o600
});
createServer(async (request, response) => {
  response.setHeader('Content-Type', 'application/json');
  const url = new URL(request.url, 'http://127.0.0.1:4199');
  if (url.pathname === '/health') {
    response.end('{}');
    return;
  }
  let body = '';
  for await (const chunk of request) {
    body += chunk;
    if (body.length > 65536) {
      response.writeHead(413);
      response.end('{}');
      return;
    }
  }
  if (url.pathname === '/v1/auth/jwt/login') {
    const input = JSON.parse(body);
    if (input.role !== 'olp' || input.jwt !== 'fixture.browser.jwt') {
      response.writeHead(403);
      response.end('{}');
      return;
    }
    response.end(
      JSON.stringify({ auth: { client_token: 'fixture-browser-vault-token' } })
    );
    return;
  }
  if (
    request.headers['x-vault-token'] !== 'fixture-browser-vault-token' ||
    url.pathname !== '/v1/secret/data/provider' ||
    !['1', '2'].includes(url.searchParams.get('version'))
  ) {
    response.writeHead(404);
    response.end('{}');
    return;
  }
  response.end(
    JSON.stringify({
      data: {
        data: { api_key: 'compatible-provider-secret' },
        metadata: {
          version: Number(url.searchParams.get('version')),
          destroyed: false,
          deletion_time: ''
        }
      }
    })
  );
}).listen(4199, '127.0.0.1');
