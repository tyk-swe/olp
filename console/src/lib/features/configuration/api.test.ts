import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  applyConfiguration,
  exportConfiguration,
  grantEnrollments,
  isCodeBindings,
  missingSecretBindings,
  pinningProviders,
  planConfiguration,
  type CodeBindings,
  type ConfigurationDocument,
  type ConfigurationExport,
  type ConfigurationPlan,
  type ExternalCredentialReference
} from './api';
import type { ProblemDetails } from '$lib/api/http';

vi.mock('$lib/features/access/session/authMiddleware', () => ({
  createAuthMiddleware: () => ({
    onRequest: ({ request }: { request: Request }) => request
  })
}));

type PortableProvider = ConfigurationDocument['providers'][number];

function provider(
  name: string,
  configuration: PortableProvider['configuration']
): PortableProvider {
  return {
    name,
    project: 'Coding',
    configuration,
    models: [],
    slots: [
      {
        name: 'default',
        credential_ref: `${name}/default`,
        enabled: true,
        is_default: true,
        position: 0,
        priority: 0,
        weight: 1,
        limits: {
          max_concurrency: null,
          requests_per_minute: null,
          tokens_per_minute: null
        },
        restrictions: {
          allowed_api_keys: [],
          allowed_models: [],
          allowed_routes: []
        }
      }
    ]
  };
}

const configuration: PortableProvider['configuration'] = {
  kind: 'openai_compatible',
  auth_mode: 'api_key',
  endpoint: 'https://fixture.example'
};
const account = '10000000-0000-4000-8000-000000000001';
const document: ConfigurationDocument = {
  api_version: 'openllmproxy.dev/config/v2',
  projects: [{ name: 'Coding' }],
  providers: [
    provider('Metered', configuration),
    provider('Archived', configuration)
  ],
  routes: [],
  pricing: null,
  code: {
    accounts: [
      {
        id: account,
        project: 'Coding',
        provider: 'Subscription',
        credential_ref: 'coding/account',
        name: 'Coding account',
        enabled: true,
        models: ['fixture-native']
      }
    ],
    pools: [
      {
        id: '10000000-0000-4000-8000-000000000002',
        project: 'Coding',
        name: 'Personal pool',
        kind: 'personal',
        owner_ref: 'coding/owner',
        accounts: { [account]: 3 },
        api_key_refs: ['coding/key']
      }
    ]
  }
};
const secretBindings = { 'Metered/default': 'fixture-secret' };
const externalBindings: Record<string, ExternalCredentialReference> = {
  'Archived/default': {
    store: 'vault',
    secret_id: 'fixture/credential',
    version: '3',
    field: 'api_key'
  }
};
const codeBindings: CodeBindings = {
  credentials: { 'coding/account': '10000000-0000-4000-8000-000000000003' },
  users: { 'coding/owner': '10000000-0000-4000-8000-000000000004' },
  api_keys: { 'coding/key': '10000000-0000-4000-8000-000000000005' }
};
const plan: ConfigurationPlan = {
  digest: 'a'.repeat(64),
  actions: [
    { kind: 'code_pool', key: 'Personal pool', action: 'create', detail: '' },
    {
      kind: 'credential',
      key: 'Subscription/default',
      action: 'enroll',
      detail: 'grant_enrollment_required'
    },
    { kind: 'credential', key: 'Metered/default', action: 'bind', detail: '' }
  ],
  blockers: [
    {
      kind: 'credential',
      key: 'Metered/default',
      action: 'blocker',
      detail: 'secret_binding_required'
    },
    {
      kind: 'plugin',
      key: 'fixture-plugin',
      action: 'blocker',
      detail: 'plugin_not_installed'
    },
    {
      kind: 'network_credential',
      key: 'Subscription/network',
      action: 'blocker',
      detail: 'secret_binding_required'
    }
  ],
  conflicts: [
    {
      kind: 'route',
      key: 'occupied-route',
      action: 'conflict',
      detail: 'project_mismatch'
    }
  ]
};
const requestId = '10000000-0000-4000-8000-000000000006';
const operations = [
  {
    name: 'export',
    execute: (signal?: AbortSignal) => exportConfiguration(signal)
  },
  {
    name: 'plan',
    execute: (signal?: AbortSignal) =>
      planConfiguration(
        document,
        secretBindings,
        signal,
        externalBindings,
        codeBindings
      )
  },
  {
    name: 'apply',
    execute: (signal?: AbortSignal) =>
      applyConfiguration(
        document,
        secretBindings,
        requestId,
        signal,
        externalBindings,
        codeBindings
      )
  }
] as const;

function respond(handler: (request: Request) => Response | Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn(handler));
}

afterEach(() => vi.unstubAllGlobals());

describe('configuration API', () => {
  it('exports the complete artifact and its canonical digest', async () => {
    const exported: ConfigurationExport = { document, digest: plan.digest };
    respond((request) => {
      expect(request.method).toBe('GET');
      expect(new URL(request.url).pathname).toBe(
        '/api/v1/configuration/export'
      );
      return Response.json(exported);
    });
    expect(await exportConfiguration()).toEqual(exported);
  });

  it('plans with exact secret, external and destination identity bindings', async () => {
    respond(async (request) => {
      expect(request.method).toBe('POST');
      expect(new URL(request.url).pathname).toBe('/api/v1/configuration/plan');
      expect(await request.json()).toEqual({
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings,
        code_bindings: codeBindings
      });
      return Response.json(plan);
    });
    expect(
      await planConfiguration(
        document,
        secretBindings,
        undefined,
        externalBindings,
        codeBindings
      )
    ).toEqual(plan);
  });

  it('applies the reviewed bindings with the caller idempotency key', async () => {
    respond(async (request) => {
      expect(request.method).toBe('POST');
      expect(new URL(request.url).pathname).toBe('/api/v1/configuration/apply');
      expect(request.headers.get('Idempotency-Key')).toBe(requestId);
      expect(await request.json()).toEqual({
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings,
        code_bindings: codeBindings
      });
      return Response.json(plan);
    });
    expect(
      await applyConfiguration(
        document,
        secretBindings,
        requestId,
        undefined,
        externalBindings,
        codeBindings
      )
    ).toEqual(plan);
  });

  it.each(operations)(
    'preserves API refusal details for $name',
    async ({ execute }) => {
      const problem: ProblemDetails = {
        type: 'urn:olp:problem:configuration',
        title: 'Promotion refused',
        status: 422,
        detail: 'A destination binding is unavailable',
        instance: '/api/v1/configuration',
        errors: {
          'code_bindings.credentials': [
            {
              code: 'unavailable',
              message: 'Enroll a current destination credential'
            }
          ]
        }
      };
      respond(() => Response.json(problem, { status: problem.status }));
      await expect(execute()).rejects.toMatchObject({
        name: 'ApiProblem',
        problem
      });
    }
  );

  it.each(operations)(
    'rejects a missing JSON response for $name',
    async ({ execute }) => {
      respond(() => new Response(null, { status: 204 }));
      await expect(execute()).rejects.toMatchObject({
        problem: { status: 502, type: 'urn:olp:problem:invalid-api-response' }
      });
    }
  );

  it.each(operations)(
    'forwards cancellation to the $name transport',
    async ({ execute }) => {
      const controller = new AbortController();
      respond((request) => {
        if (request.signal.aborted)
          return Promise.reject(request.signal.reason);
        return new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener(
            'abort',
            () => reject(request.signal.reason),
            { once: true }
          );
        });
      });
      const pending = execute(controller.signal);
      controller.abort();
      await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    }
  );
});

describe('configuration destination bindings', () => {
  it.each([
    {},
    codeBindings,
    { credentials: {} },
    { users: {} },
    { api_keys: {} }
  ])('accepts declared string-valued binding maps %j', (value) =>
    expect(isCodeBindings(value)).toBe(true)
  );

  it.each([
    null,
    undefined,
    false,
    0,
    '',
    'bindings',
    [],
    { credentials: null },
    { credentials: [] },
    { credentials: 'credential' },
    { users: { owner: 42 } },
    { api_keys: { key: false } },
    { credentials: { first: 'valid', second: null } },
    { credentials: {}, unknown: {} }
  ])('refuses malformed or undeclared bindings %j', (value) => {
    expect(isCodeBindings(value)).toBe(false);
  });
});

describe('configuration plan requirements', () => {
  it('selects missing secret references without other blockers', () => {
    expect(missingSecretBindings(plan)).toEqual([
      'Metered/default',
      'Subscription/network'
    ]);
  });

  it('selects slots requiring grant enrollment without already bound slots', () => {
    expect(grantEnrollments(plan)).toEqual(['Subscription/default']);
  });

  it('returns no credential requirements for a plan without actions or blockers', () => {
    const ready: ConfigurationPlan = {
      ...plan,
      actions: [],
      blockers: [],
      conflicts: []
    };
    expect(missingSecretBindings(ready)).toEqual([]);
    expect(grantEnrollments(ready)).toEqual([]);
  });

  it('finds every plugin provider pinning the selected digest', () => {
    const digest = `sha256:${'a'.repeat(64)}`;
    const plugin: PortableProvider['configuration'] = {
      kind: 'plugin',
      auth_mode: 'grant',
      profile_id: 'fixture-coding',
      profile_revision: digest
    };
    const artifact: ConfigurationDocument = {
      ...document,
      providers: [
        provider('First pinned', plugin),
        provider('Other build', {
          ...plugin,
          profile_revision: `sha256:${'b'.repeat(64)}`
        }),
        provider('Non-plugin', { ...configuration, profile_revision: digest }),
        provider('Second pinned', plugin)
      ]
    };
    expect(pinningProviders(artifact, digest)).toEqual([
      'First pinned',
      'Second pinned'
    ]);
    expect(pinningProviders(artifact, `sha256:${'c'.repeat(64)}`)).toEqual([]);
  });
});
