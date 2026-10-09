import { expect, type Page } from '../playwright';

// Provision feature prerequisites through the same origin, CSRF and ETag
// boundary as the console. Native source can be passed without reserializing.
export async function manage<T = Record<string, unknown>>(
  page: Page,
  method: string,
  path: string,
  options: {
    body?: unknown;
    source?: string;
    match?: string;
    etag?: string;
    idempotency?: string;
  } = {}
): Promise<{ status: number; body: T; source: string }> {
  return page.evaluate(
    async ({ method, path, options }) => {
      const session = await fetch('/api/v1/sessions/current').then((r) =>
        r.json()
      );
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token
      };
      if (options.match) {
        const current = await fetch(options.match).then((r) => r.json());
        headers['If-Match'] = `"${current.etag}"`;
      }
      if (options.etag) headers['If-Match'] = `"${options.etag}"`;
      if (options.idempotency) headers['Idempotency-Key'] = options.idempotency;
      const response = await fetch(path, {
        method,
        headers,
        body:
          options.source ??
          (options.body === undefined
            ? undefined
            : JSON.stringify(options.body))
      });
      const source = await response.text();
      return {
        status: response.status,
        body: source ? JSON.parse(source) : null,
        source
      };
    },
    { method, path, options }
  );
}

// Accounting and proxy checks need a published route; the provider and route
// onboarding journeys separately exercise each of these steps through the UI.
export async function provisionGenerationRoute(
  page: Page,
  fixture: {
    providerName: string;
    route: string;
    endpoint: string;
    model: string;
    credential: string;
    credentialSource?: 'operator' | 'caller';
  }
) {
  type Resource = {
    id: string;
    etag: string;
    items: { id: string }[];
    status: string;
  };
  async function checked(
    method: string,
    path: string,
    options: Parameters<typeof manage>[3],
    status = 200
  ) {
    const response = await manage<Resource>(page, method, path, options);
    expect(response.status, response.source).toBe(status);
    return response.body;
  }
  const provider = await checked(
    'POST',
    '/api/v1/providers',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: fixture.providerName,
        configuration: {
          kind: 'openai_compatible',
          endpoint: fixture.endpoint,
          auth_mode: 'api_key',
          ...(fixture.credentialSource
            ? { credential_source: fixture.credentialSource }
            : {})
        },
        model: fixture.model,
        credential: fixture.credential
      }
    },
    201
  );
  const path = `/api/v1/providers/${provider.id}`;
  const models = await checked('GET', `${path}/models`, {});
  await checked('PATCH', `${path}/models/${models.items[0].id}`, {
    match: path,
    body: {
      enabled: true,
      capabilities: [
        { operation: 'generation', surface: 'openai', mode: 'unary' },
        { operation: 'generation', surface: 'openai', mode: 'streaming' }
      ]
    }
  });
  const certified = await checked(
    'POST',
    `${path}/models/${models.items[0].id}/certify`,
    { match: path }
  );
  expect(certified.status).not.toBe('failed');
  await checked('POST', `${path}/activate`, {
    match: path,
    idempotency: crypto.randomUUID()
  });
  const draft = await checked(
    'POST',
    '/api/v1/route-drafts',
    {
      idempotency: crypto.randomUUID(),
      body: {
        slug: fixture.route,
        operations: ['generation'],
        fidelity: { mode: 'transformed' },
        overall_timeout_ms: 20_000,
        max_attempts: 1,
        targets: [
          {
            provider_id: provider.id,
            provider_model: fixture.model,
            priority: 0,
            weight: 1,
            timeout_ms: 15_000
          }
        ]
      }
    },
    201
  );
  await checked('POST', `/api/v1/route-drafts/${draft.id}/activate`, {
    etag: draft.etag,
    idempotency: crypto.randomUUID()
  });
  return { provider, draft };
}
