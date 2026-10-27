import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test, type APIRequestContext, type Page } from '../playwright';

const upstream = {
  origin: 'http://127.0.0.1:4191',
  model: 'fixture-voice',
  credential: 'webrtc-fixture-secret'
};
const route = 'e2e-webrtc';
const offerSDP = 'v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n';

type Resource = {
  id: string;
  etag: string;
  items: { id: string }[];
  status: string;
  secret: string;
};

async function checked(
  page: Page,
  method: string,
  path: string,
  options: Parameters<typeof manage>[3],
  status = 200
) {
  const response = await manage<Resource>(page, method, path, options);
  expect(response.status, response.source).toBe(status);
  return response.body;
}

async function mockState(request: APIRequestContext) {
  const response = await request.get(`${upstream.origin}/__test__/requests`);
  return (await response.json()) as {
    calls: string[];
    hangups: number;
    controls: number;
    unexpected: string[];
  };
}

test('browser WebRTC session is created, controlled and closed by the gateway', async ({
  page,
  request
}) => {
  await request.post(`${upstream.origin}/__test__/reset`);
  await signIn(page);
  const provider = await checked(
    page,
    'POST',
    '/api/v1/providers',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: 'WebRTC fixture upstream',
        configuration: {
          kind: 'openai',
          profile_id: 'openai-responses',
          profile_revision: '1',
          endpoint: `${upstream.origin}/v1`,
          auth_mode: 'api_key'
        },
        model: upstream.model,
        credential: upstream.credential
      }
    },
    201
  );
  const path = `/api/v1/providers/${provider.id}`;
  const models = await checked(page, 'GET', `${path}/models`, {});
  await checked(page, 'PATCH', `${path}/models/${models.items[0].id}`, {
    match: path,
    body: {
      enabled: true,
      capabilities: [
        { operation: 'realtime', surface: 'openai', mode: 'realtime' },
        { operation: 'realtime', surface: 'openai', mode: 'webrtc' }
      ]
    }
  });
  const certified = await checked(
    page,
    'POST',
    `${path}/models/${models.items[0].id}/certify`,
    { match: path }
  );
  expect(certified.status).toBe('certified');
  await checked(page, 'POST', `${path}/activate`, {
    match: path,
    idempotency: crypto.randomUUID()
  });
  const draft = await checked(
    page,
    'POST',
    '/api/v1/route-drafts',
    {
      idempotency: crypto.randomUUID(),
      body: {
        slug: route,
        operations: ['realtime'],
        fidelity: { mode: 'strict' },
        overall_timeout_ms: 30_000,
        max_attempts: 1,
        targets: [
          {
            provider_id: provider.id,
            provider_model: upstream.model,
            priority: 0,
            weight: 1,
            timeout_ms: 20_000
          }
        ]
      }
    },
    201
  );
  await checked(page, 'POST', `/api/v1/route-drafts/${draft.id}/activate`, {
    etag: draft.etag,
    idempotency: crypto.randomUUID()
  });
  const key = await checked(
    page,
    'POST',
    '/api/v1/api-keys',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: 'WebRTC browser key',
        scopes: ['inference'],
        allowed_routes: [route]
      }
    },
    201
  );

  const created = await page.evaluate(
    async ({ secret, route, sdp }) => {
      const form = new FormData();
      form.append(
        'sdp',
        new Blob([sdp], { type: 'application/sdp' }),
        'offer.sdp'
      );
      form.append(
        'session',
        new Blob([JSON.stringify({ type: 'realtime', model: route })], {
          type: 'application/json'
        })
      );
      const response = await fetch('/v1/realtime/calls', {
        method: 'POST',
        headers: { Authorization: `Bearer ${secret}` },
        body: form
      });
      return {
        status: response.status,
        location: response.headers.get('Location') ?? '',
        body: await response.text()
      };
    },
    { secret: key.secret, route, sdp: offerSDP }
  );
  expect(created.status).toBe(201);
  expect(created.body).toContain('v=0');
  expect(created.body).not.toContain('client_secret');
  expect(created.location).toMatch(/^\/v1\/realtime\/calls\//);
  expect(created.location).not.toContain('rtc_fixture');

  const settled = await mockState(request);
  expect(settled.calls).toEqual(['bound', 'bound']);
  expect(settled.controls).toBeGreaterThanOrEqual(2);
  expect(settled.unexpected).toEqual([]);

  const hangup = await page.evaluate(
    async ({ secret, location }) => {
      const response = await fetch(`${location}/hangup`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${secret}` }
      });
      return response.status;
    },
    { secret: key.secret, location: created.location }
  );
  expect(hangup).toBe(200);
  await expect
    .poll(async () => (await mockState(request)).hangups)
    .toBeGreaterThanOrEqual(2);
});
