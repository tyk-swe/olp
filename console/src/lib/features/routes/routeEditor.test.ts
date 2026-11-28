import { describe, expect, it } from 'vitest';

import {
  namedLimitRow,
  namedLimitsError
} from '#lib/features/budgets/namedLimits.js';
import {
  policyError,
  policyForm
} from '#lib/features/access/end-users/policy.js';
import { groupError } from '#lib/features/access/route-groups/policy.js';

import { emptyBehavior } from './routeBehavior';
import {
  validateRouteBehavior,
  validateRouteEditor,
  validateTargetRoles,
  type RouteEditorValues
} from './routeEditor';
import type { EditableTarget } from './routeTargetModel';

describe('route validation and serialization', () => {
  const target: EditableTarget = {
    providerModelId: 'model',
    priority: 1,
    weight: 1,
    timeoutMs: 5000
  };
  const route: RouteEditorValues = {
    slug: 'route',
    operations: ['generation'],
    overallTimeoutMs: 10000,
    maxAttempts: 2,
    targets: [target],
    fidelity: { mode: 'transformed' },
    behavior: emptyBehavior()
  };

  it.each([
    { slug: 'route', valid: true },
    { slug: 'route.name_1-test', valid: true },
    { slug: 'a'.repeat(100), valid: true },
    { slug: '', valid: false },
    { slug: 'Bad', valid: false },
    { slug: 'bad/route', valid: false },
    { slug: 'a'.repeat(101), valid: false }
  ])('validates route and policy names consistently', ({ slug, valid }) => {
    expect(validateRouteEditor({ ...route, slug }) === null).toBe(valid);
    expect(
      groupError([{ id: 'row', name: slug, routes: 'route' }]) === ''
    ).toBe(valid);
    expect(namedLimitsError([namedLimitRow(slug)], 'route') === '').toBe(valid);
    if (slug)
      expect(
        policyError({ ...policyForm(), enabled: true, limitTemplate: slug }) ===
          ''
      ).toBe(valid);
  });

  it('validates each referenced route and template without rejecting trimmed names', () => {
    expect(
      groupError([{ id: 'group', name: 'group', routes: 'valid, Bad/route' }])
    ).not.toBe('');
    expect(
      namedLimitsError(
        [namedLimitRow('valid'), namedLimitRow('Bad/template')],
        'template'
      )
    ).not.toBe('');
    expect(
      policyError({ ...policyForm(), enabled: true, limitTemplate: ' valid ' })
    ).toBe('');
    expect(
      validateRouteBehavior({
        ...route,
        behavior: {
          ...emptyBehavior(),
          fallbacks: [{ route: 'backup', on: ['exhausted'] }]
        }
      })
    ).toBeNull();
    expect(
      validateRouteBehavior({
        ...route,
        behavior: {
          ...emptyBehavior(),
          fallbacks: [{ route: 'Bad/route', on: ['exhausted'] }]
        }
      })
    ).not.toBeNull();
  });

  it.each([
    ['route', 100],
    ['template', 64]
  ] as const)(
    'enforces the %s row cap and rejects duplicate trimmed names',
    (kind, cap) => {
      const rows = Array.from({ length: cap }, (_, index) =>
        namedLimitRow(`entry-${index}`)
      );
      expect(namedLimitsError(rows, kind)).toBe('');
      expect(
        namedLimitsError([...rows, namedLimitRow('extra')], kind)
      ).not.toBe('');
      expect(
        namedLimitsError(
          [namedLimitRow('shared'), namedLimitRow(' shared ')],
          kind
        )
      ).not.toBe('');
    }
  );

  it('accepts a bounded serving route and refuses invalid basic inputs', () => {
    expect(validateRouteEditor(route)).toBeNull();
    const invalid = [
      { ...route, slug: 'Bad Slug' },
      { ...route, operations: [] },
      { ...route, targets: [] },
      { ...route, maxAttempts: 0 },
      { ...route, maxAttempts: 32768 },
      { ...route, overallTimeoutMs: 0 },
      { ...route, overallTimeoutMs: 3600001 },
      { ...route, targets: [{ ...target, priority: -1 }] },
      { ...route, targets: [{ ...target, weight: 0 }] },
      { ...route, targets: [{ ...target, timeoutMs: 10001 }] }
    ];
    for (const value of invalid)
      expect(validateRouteEditor(value)).not.toBeNull();
  });

  it('requires valid target tags and at least one serving target', () => {
    expect(
      validateTargetRoles([{ ...target, tags: ['primary', 'large'] }])
    ).toBeNull();
    expect(
      validateTargetRoles([{ ...target, tags: ['same', 'same'] }])
    ).toMatch(/distinct tags/i);
    expect(validateTargetRoles([{ ...target, tags: ['Bad tag'] }])).toMatch(
      /tags/i
    );
    expect(validateTargetRoles([{ ...target, shadowSampleRate: 0 }])).toMatch(
      /share/i
    );
    expect(validateTargetRoles([{ ...target, shadowSampleRate: 1.1 }])).toMatch(
      /share/i
    );
    expect(validateTargetRoles([{ ...target, shadowSampleRate: 0.5 }])).toMatch(
      /serves callers/i
    );
    expect(
      validateTargetRoles([target, { ...target, shadowSampleRate: 0.5 }])
    ).toBeNull();
  });

  it('refuses fallbacks, selectors, retries, affinity and cost bounds that cannot be honored', () => {
    const defaults = emptyBehavior();
    const invalid = [
      { ...defaults, fallbacks: [{ route: 'route', on: ['exhausted'] }] },
      { ...defaults, fallbacks: [{ route: 'other', on: [] }] },
      { ...defaults, selectors: [{ id: 's', when: {} }] },
      { ...defaults, selectors: [{ id: 's', when: {}, tags: ['missing'] }] },
      {
        ...defaults,
        retry: {
          timeout: {
            max_retries: 11,
            base_backoff_ms: 1,
            max_backoff_ms: 2,
            respect_retry_after: false
          }
        }
      },
      { ...defaults, affinity: { source: 'label', label: 'invalid label' } },
      { ...defaults, max_body_bytes: 0 },
      { ...defaults, budget: {} },
      { ...defaults, budget: { daily_cost_limit: '-1' } }
    ] satisfies NonNullable<RouteEditorValues['behavior']>[];
    for (const behavior of invalid)
      expect(validateRouteBehavior({ ...route, behavior })).not.toBeNull();
    expect(
      validateRouteBehavior({ ...route, selectorsError: 'Malformed selector' })
    ).toBe('Malformed selector');
    expect(validateRouteBehavior({ ...route, behavior: undefined })).toBeNull();
  });

  it('enforces the restrictions for publishing a pass-through route', () => {
    const binding: NonNullable<
      NonNullable<RouteEditorValues['behavior']>['passthrough']
    > = {
      dialect: 'bound-native',
      methods: ['GET', 'POST'],
      paths: ['/v1/widgets/*'],
      query: ['limit'],
      headers: ['X-Widget-Mode'],
      acknowledge_opaque_unmetered: true
    };
    const behavior = { ...emptyBehavior(), passthrough: binding };
    const passThrough = {
      ...route,
      operations: ['passthrough'],
      maxAttempts: 1,
      fidelity: { mode: 'strict' as const },
      behavior
    };
    expect(validateRouteEditor(passThrough)).toBeNull();
    expect(
      validateRouteEditor({ ...passThrough, operations: ['generation'] })
    ).toMatch(/only the passthrough operation/i);
    expect(
      validateRouteEditor({ ...passThrough, targets: [target, target] })
    ).toMatch(/exactly one serving connection/i);
    expect(
      validateRouteEditor({
        ...passThrough,
        targets: [{ ...target, shadowSampleRate: 0.5 }]
      })
    ).toMatch(/serves callers/i);
    expect(validateRouteEditor({ ...passThrough, maxAttempts: 2 })).toMatch(
      /exactly one attempt/i
    );
    expect(
      validateRouteEditor({ ...passThrough, fidelity: { mode: 'transformed' } })
    ).toMatch(/cannot declare translation/i);
    expect(
      validateRouteEditor({
        ...passThrough,
        behavior: {
          ...behavior,
          fallbacks: [{ route: 'backup', on: ['exhausted'] }]
        }
      })
    ).toMatch(/cannot use fallback/i);
    expect(
      validateRouteEditor({
        ...passThrough,
        behavior: {
          ...behavior,
          selectors: [{ id: 'large', when: {}, route: 'backup' }]
        }
      })
    ).toMatch(/cannot use fallback/i);
    expect(
      validateRouteEditor({
        ...passThrough,
        behavior: { ...behavior, passthrough: null }
      })
    ).toMatch(/requires a pass-through binding/i);
  });

  it('accepts bounded route and target limits and rejects values beyond them', () => {
    const boundary = {
      ...route,
      overallTimeoutMs: 3600000,
      maxAttempts: 32767,
      targets: [
        { ...target, priority: 32767, weight: 1000000, timeoutMs: 3600000 }
      ]
    };
    expect(validateRouteEditor(boundary)).toBeNull();
    expect(validateRouteEditor({ ...boundary, overallTimeoutMs: 1.5 })).toMatch(
      /overall deadline/i
    );
    expect(
      validateRouteEditor({
        ...boundary,
        targets: [{ ...target, priority: 32768 }]
      })
    ).toMatch(/priority/i);
    expect(
      validateRouteEditor({
        ...boundary,
        targets: [{ ...target, weight: 1000001 }]
      })
    ).toMatch(/weight/i);
    expect(
      validateRouteEditor({
        ...boundary,
        targets: [{ ...target, timeoutMs: 0 }]
      })
    ).toMatch(/timeout/i);
    expect(
      validateRouteBehavior({
        ...route,
        behavior: { ...emptyBehavior(), max_body_bytes: 1073741824 }
      })
    ).toBeNull();
    expect(
      validateRouteBehavior({
        ...route,
        behavior: { ...emptyBehavior(), max_body_bytes: 1073741825 }
      })
    ).toMatch(/body limit/i);
    expect(
      validateRouteBehavior({
        ...route,
        behavior: { ...emptyBehavior(), max_body_bytes: 1.5 }
      })
    ).toMatch(/body limit/i);
  });

  it('requires explicit acknowledgement before saving semantic cache settings', () => {
    const cache = {
      mode: 'semantic' as const,
      scope: 'key' as const,
      ttl_seconds: 300,
      max_entry_bytes: 1024,
      streaming: 'replay' as const
    };
    const behavior = { ...emptyBehavior(), cache };
    expect(
      validateRouteBehavior({
        ...route,
        fidelity: { mode: 'strict' },
        behavior
      })
    ).toMatch(/require a transformed route/i);
    expect(validateRouteBehavior({ ...route, behavior })).toMatch(
      /embedding route and the unencrypted-vector acknowledgement/i
    );
    expect(
      validateRouteBehavior({
        ...route,
        behavior: {
          ...behavior,
          cache: { ...cache, embedding_route: 'embeddings' }
        }
      })
    ).toMatch(/unencrypted-vector acknowledgement/i);
    expect(
      validateRouteBehavior({
        ...route,
        behavior: {
          ...behavior,
          cache: {
            ...cache,
            embedding_route: 'embeddings',
            allow_unsealed_vectors: true
          }
        }
      })
    ).toBeNull();
  });
});

describe('gateway tool execution', () => {
  const values: RouteEditorValues = {
    slug: 'workflow',
    operations: ['generation'],
    overallTimeoutMs: 120000,
    maxAttempts: 2,
    targets: [
      { providerModelId: 'model', priority: 1, weight: 1, timeoutMs: 60000 }
    ],
    fidelity: { mode: 'transformed' },
    behavior: {
      ...emptyBehavior(),
      tool_execution: 'gateway',
      responses_mcp_items: true,
      toolsets: ['0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607']
    }
  };

  it('requires transformed fidelity for gateway execution', () => {
    expect(
      validateRouteEditor({ ...values, fidelity: { mode: 'strict' } })
    ).toMatch(/transformed/i);
    expect(validateRouteEditor(values)).toBeNull();
  });
});

describe('registered operation discovery', () => {
  it('makes registered operations and native dialects selectable without losing legacy workflows', async () => {
    const { discoveredOperations, discoveredSurfaces, inspectionDialects } =
      await import('./inspectionDialects');
    const registered = [
      {
        id: 'fixture-embed',
        revision: '1',
        operation: 'fixture-operation',
        operation_revision: '1',
        surface: 'gemini',
        mode: 'unary',
        label: 'Fixture embeddings',
        request_schema: {},
        result_schema: {},
        documentation: '',
        evidence: ''
      }
    ];
    const operations = discoveredOperations(registered);
    expect(operations.map(([id]) => id)).toContain('fixture-operation');
    expect(operations.map(([id]) => id)).toEqual(
      expect.arrayContaining([
        'generation',
        'video_create',
        'realtime',
        'decision'
      ])
    );
    expect(discoveredSurfaces('fixture-operation', registered)).toEqual(
      expect.arrayContaining(['gemini', 'native'])
    );
    expect(
      inspectionDialects('fixture-operation', 'native', registered)
    ).toEqual(['fixture-embed']);
    expect(inspectionDialects('generation', 'openai', registered)).toEqual(
      inspectionDialects('generation', 'openai')
    );
  });
});
