// Loopback-only issuer for the explicitly tagged Go identity test binary.
import { createServer } from 'node:http';
import { createHash, generateKeyPairSync, randomUUID, sign } from 'node:crypto';

const issuer = 'http://127.0.0.1:4186';
const { privateKey, publicKey } = generateKeyPairSync('ed25519');
const jwk = {
  ...publicKey.export({ format: 'jwk' }),
  kid: 'browser',
  alg: 'EdDSA',
  use: 'sig'
};
const codes = new Map();
const encode = (value) =>
  Buffer.from(JSON.stringify(value)).toString('base64url');
createServer(async (request, response) => {
  const url = new URL(request.url, issuer);
  response.setHeader('Content-Type', 'application/json');
  if (url.pathname === '/.well-known/openid-configuration') {
    response.end(
      JSON.stringify({
        issuer,
        authorization_endpoint: issuer + '/authorize',
        token_endpoint: issuer + '/token',
        jwks_uri: issuer + '/jwks',
        response_types_supported: ['code'],
        subject_types_supported: ['public'],
        id_token_signing_alg_values_supported: ['EdDSA'],
        code_challenge_methods_supported: ['S256']
      })
    );
  } else if (url.pathname === '/jwks') {
    response.end(JSON.stringify({ keys: [jwk] }));
  } else if (url.pathname === '/authorize') {
    const redirect = new URL(url.searchParams.get('redirect_uri'));
    if (
      !['http://127.0.0.1:4182', 'http://127.0.0.1:4183'].includes(
        redirect.origin
      ) ||
      redirect.pathname !== '/api/v1/oidc/callback' ||
      url.searchParams.get('code_challenge_method') !== 'S256'
    ) {
      response.writeHead(400).end('{}');
      return;
    }
    const code = randomUUID();
    codes.set(code, {
      nonce: url.searchParams.get('nonce'),
      challenge: url.searchParams.get('code_challenge'),
      client: url.searchParams.get('client_id'),
      expires: Date.now() + 60_000
    });
    redirect.searchParams.set('code', code);
    redirect.searchParams.set('state', url.searchParams.get('state'));
    response.writeHead(302, { Location: redirect.toString() }).end();
  } else if (url.pathname === '/token') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const form = new URLSearchParams(Buffer.concat(chunks).toString());
    const code = codes.get(form.get('code'));
    codes.delete(form.get('code'));
    const auth = Buffer.from(
      (request.headers.authorization ?? '').replace('Basic ', ''),
      'base64'
    ).toString();
    const challenge = createHash('sha256')
      .update(form.get('code_verifier') ?? '')
      .digest('base64url');
    if (
      !code ||
      code.expires < Date.now() ||
      challenge !== code.challenge ||
      auth !== code.client + ':write-only-browser-secret'
    ) {
      response.writeHead(400).end(JSON.stringify({ error: 'invalid_grant' }));
      return;
    }
    const now = Math.floor(Date.now() / 1000);
    const payload = {
      iss: issuer,
      aud: code.client,
      sub: 'browser-member',
      email: 'sso@example.com',
      email_verified: true,
      name: 'SSO Member',
      nonce: code.nonce,
      auth_time: now,
      iat: now,
      exp: now + 60
    };
    const signed =
      encode({ alg: 'EdDSA', kid: 'browser' }) + '.' + encode(payload);
    const token =
      signed +
      '.' +
      sign(null, Buffer.from(signed), privateKey).toString('base64url');
    response.end(
      JSON.stringify({
        access_token: 'browser-access-token',
        token_type: 'Bearer',
        id_token: token
      })
    );
  } else response.writeHead(404).end('{}');
}).listen(4186, '127.0.0.1');
