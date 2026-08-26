import type { Page } from '../playwright';

/**
 * Deterministic seed data for the documentation screenshot suite. The records
 * model a small but realistic production installation: three active providers,
 * three published routes with a reviewed draft, scoped API keys, and a healthy
 * usage pipeline. No live backend is involved; every response is mocked.
 */

const ids = {
  user: '01980100-0000-7000-8000-000000000001',
  generation: '01980100-0000-7000-8000-000000000002',
  providers: {
    openai: '01980100-0000-7000-8000-000000000101',
    anthropic: '01980100-0000-7000-8000-000000000102',
    vertex: '01980100-0000-7000-8000-000000000103'
  },
  models: {
    gpt: '01980100-0000-7000-8000-000000000201',
    claude: '01980100-0000-7000-8000-000000000202',
    gemini: '01980100-0000-7000-8000-000000000203'
  },
  routes: {
    support: '01980100-0000-7000-8000-000000000301',
    code: '01980100-0000-7000-8000-000000000302',
    embeddings: '01980100-0000-7000-8000-000000000303'
  },
  drafts: {
    vision: '01980100-0000-7000-8000-000000000351'
  },
  keys: {
    production: '01980100-0000-7000-8000-000000000401',
    staging: '01980100-0000-7000-8000-000000000402',
    partner: '01980100-0000-7000-8000-000000000403'
  }
};

const created = '2026-06-30T09:14:00Z';
export const SCREENSHOT_NOW = '2026-07-01T12:00:00.000Z';
const screenshotNowMs = Date.parse(SCREENSHOT_NOW);

function minutesAgo(minutes: number): string {
  return new Date(screenshotNowMs - minutes * 60_000).toISOString();
}

/** Session + setup mocks required by the application shell on every page. */
export async function mockShell(page: Page) {
  await page.route('**/api/v1/setup/status', async (route) => {
    await route.fulfill({ json: { setup_required: false } });
  });
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: {
          id: ids.user,
          email: 'owner@example.com',
          display_name: 'Ada Lovelace',
          role: 'owner'
        },
        csrf_token: 'csrf-screenshots',
        installation_name: 'Local installation'
      }
    });
  });
}

function providerSummary(input: {
  id: string;
  name: string;
  kind: string;
  modelCount: number;
  enabledModelCount: number;
  authMode?: string;
  endpoint?: string | null;
  cloudRegion?: string | null;
  cloudProject?: string | null;
  revision?: number;
}) {
  const capabilityCount = input.enabledModelCount * 2;
  return {
    id: input.id,
    name: input.name,
    kind: input.kind,
    state: 'active',
    auth_mode: input.authMode ?? 'api_key',
    connector_ready: true,
    endpoint: input.endpoint ?? null,
    api_version: null,
    cloud_region: input.cloudRegion ?? null,
    cloud_project: input.cloudProject ?? null,
    deployment: null,
    active_revision: input.revision ?? 3,
    pending_activation: false,
    draft_credential_id: null,
    draft_credential_version: 3,
    runtime_credential_id: null,
    runtime_credential_version: 3,
    last_probe_at: minutesAgo(42),
    last_probe_status: 'succeeded',
    last_probe_detail: 'Upstream reachable',
    etag: `${input.id}-etag`,
    created_at: created,
    updated_at: minutesAgo(180),
    model_count: input.modelCount,
    enabled_model_count: input.enabledModelCount,
    capability_count: capabilityCount,
    certified_capability_count: capabilityCount
  };
}

const providerItems = [
  providerSummary({
    id: ids.providers.openai,
    name: 'openai-production',
    kind: 'openai',
    modelCount: 6,
    enabledModelCount: 4,
    revision: 5
  }),
  providerSummary({
    id: ids.providers.anthropic,
    name: 'anthropic-production',
    kind: 'anthropic',
    modelCount: 3,
    enabledModelCount: 2,
    revision: 3
  }),
  providerSummary({
    id: ids.providers.vertex,
    name: 'vertex-gemini',
    kind: 'vertex',
    modelCount: 2,
    enabledModelCount: 2,
    authMode: 'adc',
    cloudRegion: 'us-central1',
    cloudProject: 'acme-ml-prod',
    revision: 2
  })
];

export async function mockProviders(page: Page) {
  await page.route('**/api/v1/providers*', async (route) => {
    await route.fulfill({ json: { items: providerItems, next_cursor: null } });
  });
}

function routeTarget(input: {
  id: string;
  position: number;
  providerId: string;
  providerName: string;
  providerModelId: string;
  providerModel: string;
  weight: number;
}) {
  return {
    id: input.id,
    position: input.position,
    priority: 1,
    provider_id: input.providerId,
    provider_model: input.providerModel,
    provider_model_id: input.providerModelId,
    provider_name: input.providerName,
    timeout_ms: 45_000,
    weight: input.weight
  };
}

function routeRevision(input: {
  id: string;
  routeId: string;
  slug: string;
  revision: number;
  operations: string[];
  targets: ReturnType<typeof routeTarget>[];
  activatedAt: string;
}) {
  return {
    id: input.id,
    route_id: input.routeId,
    slug: input.slug,
    revision: input.revision,
    source_draft_id: `${input.routeId}-draft`,
    operations: input.operations,
    overall_timeout_ms: 60_000,
    max_attempts: 2,
    targets: input.targets,
    activated_by: ids.user,
    activated_at: input.activatedAt
  };
}

const supportTargets = [
  routeTarget({
    id: '01980100-0000-7000-8000-000000000501',
    position: 1,
    providerId: ids.providers.openai,
    providerName: 'openai-production',
    providerModelId: ids.models.gpt,
    providerModel: 'gpt-5.4',
    weight: 80
  }),
  routeTarget({
    id: '01980100-0000-7000-8000-000000000502',
    position: 2,
    providerId: ids.providers.anthropic,
    providerName: 'anthropic-production',
    providerModelId: ids.models.claude,
    providerModel: 'claude-sonnet-4-5',
    weight: 20
  })
];

const activeRouteItems = [
  {
    id: ids.routes.support,
    slug: 'support-chat',
    created_at: created,
    revision_count: 4,
    latest_revision: routeRevision({
      id: '01980100-0000-7000-8000-000000000511',
      routeId: ids.routes.support,
      slug: 'support-chat',
      revision: 4,
      operations: ['generation'],
      targets: supportTargets,
      activatedAt: minutesAgo(60 * 26)
    })
  },
  {
    id: ids.routes.code,
    slug: 'code-assistant',
    created_at: created,
    revision_count: 2,
    latest_revision: routeRevision({
      id: '01980100-0000-7000-8000-000000000512',
      routeId: ids.routes.code,
      slug: 'code-assistant',
      revision: 2,
      operations: ['generation'],
      targets: [
        routeTarget({
          id: '01980100-0000-7000-8000-000000000503',
          position: 1,
          providerId: ids.providers.anthropic,
          providerName: 'anthropic-production',
          providerModelId: ids.models.claude,
          providerModel: 'claude-sonnet-4-5',
          weight: 100
        })
      ],
      activatedAt: minutesAgo(60 * 50)
    })
  },
  {
    id: ids.routes.embeddings,
    slug: 'embeddings-index',
    created_at: created,
    revision_count: 1,
    latest_revision: routeRevision({
      id: '01980100-0000-7000-8000-000000000513',
      routeId: ids.routes.embeddings,
      slug: 'embeddings-index',
      revision: 1,
      operations: ['embeddings'],
      targets: [
        routeTarget({
          id: '01980100-0000-7000-8000-000000000504',
          position: 1,
          providerId: ids.providers.openai,
          providerName: 'openai-production',
          providerModelId: ids.models.gpt,
          providerModel: 'text-embedding-3-large',
          weight: 100
        })
      ],
      activatedAt: minutesAgo(60 * 120)
    })
  }
];

const routeDraftItems = [
  {
    id: ids.drafts.vision,
    based_on_revision_id: null,
    slug: 'vision-triage',
    state: 'validated',
    operations: ['generation'],
    targets: [
      routeTarget({
        id: '01980100-0000-7000-8000-000000000505',
        position: 1,
        providerId: ids.providers.vertex,
        providerName: 'vertex-gemini',
        providerModelId: ids.models.gemini,
        providerModel: 'gemini-2.5-pro',
        weight: 100
      })
    ],
    overall_timeout_ms: 60_000,
    max_attempts: 2,
    etag: `${ids.drafts.vision}-etag`,
    created_at: created,
    updated_at: minutesAgo(60 * 5)
  }
];

export async function mockRoutes(page: Page) {
  await page.route('**/api/v1/routes*', async (route) => {
    await route.fulfill({
      json: { items: activeRouteItems, next_cursor: null }
    });
  });
  await page.route('**/api/v1/route-drafts*', async (route) => {
    await route.fulfill({
      json: { items: routeDraftItems, next_cursor: null }
    });
  });
}

const apiKeyItems = [
  {
    id: ids.keys.production,
    name: 'production-web-app',
    lookup_id: 'olp_live_7f3a9c2e',
    scopes: ['inference'],
    allowed_routes: [],
    requests_per_minute: 600,
    tokens_per_minute: 240_000,
    max_concurrency: 64,
    expires_at: null,
    revoked_at: null,
    rotated_at: null,
    created_by: ids.user,
    created_by_email: 'owner@example.com',
    created_at: created,
    etag: `${ids.keys.production}-etag`
  },
  {
    id: ids.keys.staging,
    name: 'staging-ci',
    lookup_id: 'olp_test_41bd8e07',
    scopes: ['inference'],
    allowed_routes: ['support-chat'],
    requests_per_minute: 120,
    tokens_per_minute: 24_000,
    max_concurrency: 16,
    expires_at: null,
    revoked_at: null,
    rotated_at: '2026-07-06T11:32:00Z',
    created_by: ids.user,
    created_by_email: 'owner@example.com',
    created_at: '2026-06-12T15:48:00Z',
    etag: `${ids.keys.staging}-etag`
  },
  {
    id: ids.keys.partner,
    name: 'partner-catalog-readonly',
    lookup_id: 'olp_live_c9d21f55',
    scopes: ['models_read'],
    allowed_routes: [],
    requests_per_minute: 60,
    tokens_per_minute: null,
    max_concurrency: 8,
    expires_at: '2026-12-31T23:59:59Z',
    revoked_at: null,
    rotated_at: null,
    created_by: ids.user,
    created_by_email: 'owner@example.com',
    created_at: '2026-07-02T08:05:00Z',
    etag: `${ids.keys.partner}-etag`
  }
];

export async function mockApiKeys(page: Page) {
  await page.route('**/api/v1/api-keys*', async (route) => {
    await route.fulfill({ json: { items: apiKeyItems, next_cursor: null } });
  });
}

function requestSummary(input: {
  id: string;
  route: string;
  operation: string;
  surface: string;
  startedMinutesAgo: number;
  latencyMs: number;
  statusCode: number;
  inputTokens: number;
  outputTokens: number;
  cost: string;
}) {
  const started = minutesAgo(input.startedMinutesAgo);
  return {
    id: input.id,
    runtime_generation_id: ids.generation,
    api_key_id: ids.keys.production,
    route: input.route,
    operation: input.operation,
    surface: input.surface,
    started_at: started,
    completed_at: new Date(
      new Date(started).getTime() + input.latencyMs
    ).toISOString(),
    status_code: input.statusCode,
    error_class: null,
    total_latency_ms: input.latencyMs,
    first_byte_ms: Math.round(input.latencyMs * 0.4),
    attempt_count: 1,
    input_tokens: input.inputTokens,
    output_tokens: input.outputTokens,
    cached_input_tokens: 0,
    estimated_cost: input.cost,
    unpriced: false,
    usage_complete: true
  };
}

export async function mockRecentRequests(page: Page) {
  const items = [
    requestSummary({
      id: '01980100-0000-7000-8000-000000000601',
      route: 'support-chat',
      operation: 'generation',
      surface: 'openai',
      startedMinutesAgo: 2,
      latencyMs: 812,
      statusCode: 200,
      inputTokens: 1_482,
      outputTokens: 213,
      cost: '0.00812'
    }),
    requestSummary({
      id: '01980100-0000-7000-8000-000000000602',
      route: 'code-assistant',
      operation: 'generation',
      surface: 'anthropic',
      startedMinutesAgo: 5,
      latencyMs: 1_940,
      statusCode: 200,
      inputTokens: 3_204,
      outputTokens: 891,
      cost: '0.03120'
    }),
    requestSummary({
      id: '01980100-0000-7000-8000-000000000603',
      route: 'embeddings-index',
      operation: 'embeddings',
      surface: 'openai',
      startedMinutesAgo: 9,
      latencyMs: 148,
      statusCode: 200,
      inputTokens: 512,
      outputTokens: 0,
      cost: '0.00007'
    }),
    requestSummary({
      id: '01980100-0000-7000-8000-000000000604',
      route: 'support-chat',
      operation: 'generation',
      surface: 'openai',
      startedMinutesAgo: 14,
      latencyMs: 655,
      statusCode: 200,
      inputTokens: 1_120,
      outputTokens: 176,
      cost: '0.00640'
    }),
    requestSummary({
      id: '01980100-0000-7000-8000-000000000605',
      route: 'support-chat',
      operation: 'generation',
      surface: 'gemini',
      startedMinutesAgo: 21,
      latencyMs: 704,
      statusCode: 200,
      inputTokens: 980,
      outputTokens: 188,
      cost: '0.00571'
    })
  ];
  await page.route('**/api/v1/requests*', async (route) => {
    await route.fulfill({ json: { data: items, next_cursor: null } });
  });
}
