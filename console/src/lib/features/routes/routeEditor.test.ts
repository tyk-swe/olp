import { describe, expect, it } from 'vitest';
import type { ProviderModelInventory } from '$lib/features/providers/models';
import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput,
  certifiedCapabilities,
  eligibleTargetTuples,
  missingTargetOperations,
  modesFor,
  operationOptions,
  routeEligibilityWarnings,
  surfacesFor,
  toRouteModelOptions,
  validateRouteEditor,
  type EditableTarget,
  type RouteEditorValues,
  type RouteModelOption
} from '$lib/features/routes/routeEditor';

const target: EditableTarget = {
  providerModelId: 'model-a',
  priority: 1,
  weight: 100,
  timeoutMs: 60_000
};

const validEditor: RouteEditorValues = {
  slug: 'support-chat-v2',
  operations: ['generation'],
  overallTimeoutMs: 120_000,
  maxAttempts: 1,
  targets: [target]
};

const modelOptions: RouteModelOption[] = [
  {
    id: 'model-a',
    providerId: 'provider-a',
    providerName: 'Primary',
    upstreamModel: 'model-upstream-a',
    label: 'Primary · Model A',
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'streaming',
        source: 'certified',
        certified_at: '2026-07-12T12:00:00Z'
      },
      {
        operation: 'embeddings',
        surface: 'openai',
        mode: 'unary',
        source: 'declared'
      },
      {
        operation: 'token_count',
        surface: 'anthropic',
        mode: 'unary',
        source: 'certified'
      }
    ]
  },
  {
    id: 'model-b',
    providerId: 'provider-b',
    providerName: 'Fallback',
    upstreamModel: 'model-upstream-b',
    label: 'Fallback · Model B',
    capabilities: [
      {
        operation: 'embeddings',
        surface: 'openai',
        mode: 'unary',
        source: 'certified'
      }
    ]
  }
];

describe('Route Studio operation policy', () => {
  it('keeps installation-local model operations out of routed choices', () => {
    const operations = operationOptions.map(([operation]) => operation);
    expect(operations).toContain('generation');
    expect(operations).toContain('video_delete');
    expect(operations).not.toContain('model_list');
    expect(operations).not.toContain('model_get');
  });

  it.each([
    ['generation', ['openai', 'anthropic', 'gemini']],
    ['token_count', ['openai', 'anthropic', 'gemini']],
    ['embeddings', ['openai']],
    ['video_create', ['openai']]
  ])('selects the current surfaces for %s', (operation, expected) => {
    expect(surfacesFor(operation)).toEqual(expected);
  });

  it.each([
    ['generation', ['unary', 'streaming']],
    ['image_edit', ['unary', 'streaming']],
    ['transcription', ['unary', 'streaming']],
    ['video_create', ['async']],
    ['embeddings', ['unary']],
    ['video_delete', ['unary']]
  ])('selects the current transport modes for %s', (operation, expected) => {
    expect(modesFor(operation)).toEqual(expected);
  });
});

describe('Route Studio model eligibility', () => {
  it('normalizes provider inventory without losing capability provenance', () => {
    const inventory: ProviderModelInventory[] = [
      {
        available: true,
        metadata: {
          canonical_model: null,
          context_length: null,
          data_collection: null,
          deployment: null,
          input_modalities: [],
          max_output_tokens: null,
          observed_at: null,
          output_modalities: [],
          quantization: null,
          region: null,
          source: null,
          supported_parameters: null,
          zero_data_retention: null
        },
        provider_id: 'provider-a',
        provider_name: 'Primary',
        provider_kind: 'openai',
        model: {
          id: 'model-a',
          upstream_model: 'gpt-test',
          display_name: 'GPT Test',
          enabled: true,
          discovered_at: '2026-07-12T12:00:00Z',
          capabilities: modelOptions[0].capabilities
        }
      }
    ];

    expect(toRouteModelOptions(inventory)).toEqual([
      {
        id: 'model-a',
        providerId: 'provider-a',
        providerName: 'Primary',
        upstreamModel: 'gpt-test',
        label: 'Primary · GPT Test',
        capabilities: modelOptions[0].capabilities
      }
    ]);
  });

  it('uses only certified capabilities selected by the route', () => {
    const operations = ['generation', 'embeddings'];
    expect(
      certifiedCapabilities(target, modelOptions, operations)
    ).toMatchObject([{ operation: 'generation', source: 'certified' }]);
    expect(eligibleTargetTuples(target, modelOptions, operations)).toEqual([
      'generation · openai · streaming'
    ]);
    expect(missingTargetOperations(target, modelOptions, operations)).toEqual([
      'embeddings'
    ]);
  });

  it('warns only when no selected target certifies an operation', () => {
    const targets = [target, { ...target, providerModelId: 'model-b' }];
    expect(
      routeEligibilityWarnings(targets, modelOptions, [
        'generation',
        'embeddings',
        'moderation'
      ])
    ).toEqual(['moderation']);
  });
});

describe('Route Studio editor validation', () => {
  it('accepts the current valid slug, attempt, and target contract', () => {
    expect(validateRouteEditor(validEditor)).toBeNull();
    expect(
      validateRouteEditor({ ...validEditor, slug: `a${'b'.repeat(62)}` })
    ).toBeNull();
  });

  it.each([
    '',
    'Uppercase',
    '.leading-dot',
    'trailing-hyphen-',
    'double--hyphen',
    'contains.dot',
    'contains_underscore',
    'contains/slash',
    `a${'b'.repeat(63)}`
  ])('rejects invalid route slug %j', (slug) => {
    expect(validateRouteEditor({ ...validEditor, slug })).toContain(
      'lowercase letters'
    );
  });

  it('requires operations and targets before attempt validation', () => {
    expect(validateRouteEditor({ ...validEditor, operations: [] })).toBe(
      'Select at least one supported operation.'
    );
    expect(validateRouteEditor({ ...validEditor, targets: [] })).toBe(
      'Add at least one eligible provider model target.'
    );
  });

  it('permits multiple credential attempts per target and priority zero', () => {
    expect(
      validateRouteEditor({
        ...validEditor,
        maxAttempts: 3,
        targets: [{ ...target, priority: 0 }]
      })
    ).toBeNull();
  });

  it.each([0, 32768])(
    'rejects maximum attempt count %s for one target',
    (maxAttempts) => {
      expect(validateRouteEditor({ ...validEditor, maxAttempts })).toContain(
        'between 1 and 32767'
      );
    }
  );

  it.each([{ priority: -1 }, { weight: 0 }, { timeoutMs: 99 }])(
    'rejects an invalid target bound: %o',
    (override) => {
      expect(
        validateRouteEditor({
          ...validEditor,
          targets: [{ ...target, ...override }]
        })
      ).toBe(
        'Every target needs a priority from 0 to 65535, a positive weight, and a timeout of at least 100 ms.'
      );
    }
  );
});

describe('Route Studio API payloads', () => {
  const values: RouteEditorValues = {
    ...validEditor,
    operations: ['generation', 'embeddings'],
    targets: [
      target,
      { ...target, providerModelId: 'model-b', priority: 2, weight: 25 }
    ],
    maxAttempts: 2
  };

  it('maps new targets from inventory IDs to provider and upstream model identity', () => {
    expect(buildCreateRouteDraftInput(values, modelOptions)).toEqual({
      slug: 'support-chat-v2',
      operations: ['generation', 'embeddings'],
      overall_timeout_ms: 120_000,
      max_attempts: 2,
      targets: [
        {
          provider_id: 'provider-a',
          provider_model: 'model-upstream-a',
          priority: 1,
          weight: 100,
          timeout_ms: 60_000
        },
        {
          provider_id: 'provider-b',
          provider_model: 'model-upstream-b',
          priority: 2,
          weight: 25,
          timeout_ms: 60_000
        }
      ]
    });
  });

  it('keeps existing targets anchored by provider-model ID', () => {
    expect(buildReplaceRouteDraftInput(values)).toEqual({
      slug: 'support-chat-v2',
      operations: ['generation', 'embeddings'],
      overall_timeout_ms: 120_000,
      max_attempts: 2,
      targets: [
        {
          provider_model_id: 'model-a',
          priority: 1,
          weight: 100,
          timeout_ms: 60_000
        },
        {
          provider_model_id: 'model-b',
          priority: 2,
          weight: 25,
          timeout_ms: 60_000
        }
      ]
    });
  });
});
