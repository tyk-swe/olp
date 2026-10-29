import { describe, expect, it } from 'vitest';

import {
  buildCreateRouteDraftInput,
  buildContentPolicy,
  buildReplaceRouteDraftInput,
  certifiedCapabilities,
  emptyBehavior,
  eligibleTargetTuples,
  fidelityLabel,
  hasOutputRules,
  missingTargetOperations,
  modesFor,
  parseSelectors,
  policyRulesFrom,
  routeEligibilityWarnings,
  storedTargetLabel,
  surfacesFor,
  targetUnavailable,
  validateContentPolicy,
  validateRouteBehavior,
  validateRouteEditor,
  validateTargetRoles,
  type EditablePolicyRule,
  type EditableTarget,
  type RouteEditorValues,
  type RouteModelOption
} from './routeEditor';

describe('modesFor', () => {
  it('offers webrtc as its own realtime simulation mode', () => {
    expect(modesFor('realtime')).toEqual(['realtime', 'webrtc']);
  });

  it('keeps non-realtime operations on their existing modes', () => {
    expect(modesFor('generation')).toEqual(['unary', 'streaming']);
    expect(modesFor('embeddings')).toEqual(['unary']);
  });
});

describe('route selector input', () => {
  it('accepts empty input and valid model and plugin constraints', () => {
    expect(parseSelectors('  ')).toEqual([]);
    const selectors = [
      {
        id: 'large',
        when: {
          operations: ['generation'],
          streaming: true,
          min_input_tokens: 10,
          classifier: {
            route: 'classifier',
            labels: ['technical'],
            timeout_ms: 1000,
            min_score: 0.8
          }
        },
        tags: ['large']
      },
      { id: 'plugin', when: { plugin: { digest: 'digest' } }, route: 'other' }
    ];
    expect(parseSelectors(JSON.stringify(selectors))).toEqual(selectors);
  });

  it.each([
    '{',
    '{}',
    '[{"id":"one"}]',
    '[{"id":"one","when":{"streaming":"yes"}}]',
    '[{"id":"one","when":{"min_input_tokens":1.5}}]',
    '[{"id":"one","when":{"operations":[1]}}]',
    '[{"id":"one","when":{"unknown":true}}]',
    '[{"id":"one","when":{"plugin":{"digest":"x","other":1}}}]',
    '[{"id":"one","when":{"classifier":{"route":"x","labels":["ok"],"timeout_ms":1,"extra":true}}}]',
    '[{"id":"one","when":{},"tags":[1]}]',
    '[{"id":"one","when":{},"other":true}]'
  ])('refuses malformed selector input %s', (text) => {
    expect(parseSelectors(text)).toMatch(/selectors must be/i);
  });
});

describe('content policy editing', () => {
  const block: EditablePolicyRule = {
    id: 'block_secret',
    phase: 'input',
    action: 'block',
    pattern: 'secret',
    replacement: ''
  };
  const redact: EditablePolicyRule = {
    id: 'redact_email',
    phase: 'output',
    action: 'redact',
    pattern: 'email',
    replacement: '[redacted]'
  };

  it('round trips block and output-redaction rules without adding a block replacement', () => {
    const policy = buildContentPolicy([block, redact]);
    expect(policy).toEqual({
      rules: [
        {
          id: block.id,
          phase: block.phase,
          action: block.action,
          pattern: block.pattern
        },
        {
          id: redact.id,
          phase: redact.phase,
          action: redact.action,
          pattern: redact.pattern,
          replacement: redact.replacement
        }
      ]
    });
    expect(policyRulesFrom(policy)).toEqual([block, redact]);
    expect(hasOutputRules(policyRulesFrom(policy))).toBe(true);
    expect(buildContentPolicy([])).toBeNull();
    expect(hasOutputRules([block])).toBe(false);
    expect(validateContentPolicy([block, redact])).toBeNull();
  });

  it.each([
    { rules: [{ ...block, id: '9invalid' }], problem: /rule id/i },
    { rules: [block, block], problem: /more than once/i },
    { rules: [{ ...block, pattern: '' }], problem: /pattern/i },
    { rules: [{ ...block, pattern: 'é'.repeat(257) }], problem: /pattern/i },
    {
      rules: [{ ...redact, replacement: 'x'.repeat(129) }],
      problem: /replacement/i
    },
    {
      rules: Array.from({ length: 65 }, (_, i) => ({ ...block, id: `r${i}` })),
      problem: /at most/i
    },
    {
      rules: Array.from({ length: 33 }, (_, i) => ({
        ...block,
        id: `r${i}`,
        pattern: 'x'.repeat(512)
      })),
      problem: /combined/i
    }
  ])('refuses invalid rule sets', ({ rules, problem }) => {
    expect(validateContentPolicy(rules)).toMatch(problem);
  });
});

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
    contentPolicyRules: [],
    fidelity: { mode: 'transformed' },
    behavior: emptyBehavior()
  };

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
        contentPolicyRules: [
          {
            id: 'secret',
            phase: 'input',
            action: 'block',
            pattern: 'secret',
            replacement: ''
          }
        ]
      })
    ).toMatch(/cannot carry a content policy/i);
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

  it('preserves route policy and target roles in create and edit requests', () => {
    const behavior = {
      ...emptyBehavior(),
      fallbacks: [{ route: 'backup', on: ['exhausted' as const] }],
      caller_cost_exempt: false,
      max_body_bytes: 4096
    };
    const values = {
      ...route,
      projectId: 'project',
      behavior,
      targets: [
        { ...target, tags: ['primary'] },
        { ...target, shadowSampleRate: 0.5 }
      ]
    };
    const models: RouteModelOption[] = [
      {
        id: 'model',
        providerId: 'provider',
        providerName: 'Provider',
        upstreamModel: 'upstream',
        label: 'Provider model',
        capabilities: [],
        lifecycle: null
      }
    ];
    expect(buildCreateRouteDraftInput(values, models)).toMatchObject({
      project_id: 'project',
      fallbacks: behavior.fallbacks,
      caller_cost_exempt: false,
      max_body_bytes: 4096,
      targets: [
        {
          provider_id: 'provider',
          provider_model: 'upstream',
          tags: ['primary'],
          shadow: null
        },
        { shadow: { sample_rate: 0.5 } }
      ]
    });
    expect(buildReplaceRouteDraftInput(values)).toMatchObject({
      fallbacks: behavior.fallbacks,
      targets: [
        { provider_model_id: 'model', tags: ['primary'] },
        { shadow: { sample_rate: 0.5 } }
      ]
    });
    expect(() => buildCreateRouteDraftInput(values, [])).toThrow(/inventory/i);
  });
});

describe('target and model availability', () => {
  const target: EditableTarget = {
    providerModelId: 'model',
    priority: 1,
    weight: 1,
    timeoutMs: 1000
  };
  const model: RouteModelOption = {
    id: 'model',
    providerId: 'provider',
    providerName: 'Provider',
    upstreamModel: 'upstream',
    label: 'Provider model',
    lifecycle: null,
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'unary',
        source: 'certified'
      },
      {
        operation: 'embeddings',
        surface: 'openai',
        mode: 'unary',
        source: 'declared'
      }
    ]
  };

  it('warns about operations with no certified target', () => {
    expect(
      certifiedCapabilities(target, [model], ['generation', 'embeddings'])
    ).toEqual([model.capabilities[0]]);
    expect(
      missingTargetOperations(target, [model], ['generation', 'embeddings'])
    ).toEqual(['embeddings']);
    expect(eligibleTargetTuples(target, [model], ['generation'])).toEqual([
      'generation · openai · unary'
    ]);
    expect(
      routeEligibilityWarnings([target], [model], ['generation', 'embeddings'])
    ).toEqual(['embeddings']);
    expect(routeEligibilityWarnings([], [model], ['generation'])).toEqual([
      'generation'
    ]);
  });

  it('retains an unavailable stored target and labels fidelity honestly', () => {
    const unavailable = {
      ...target,
      stored: {
        providerModelId: 'model',
        providerName: 'Provider',
        providerModel: 'upstream',
        available: false
      }
    };
    expect(storedTargetLabel(unavailable)).toBe('Provider · upstream');
    expect(targetUnavailable(unavailable)).toBe(true);
    expect(
      storedTargetLabel({ ...unavailable, providerModelId: 'other' })
    ).toBe('Unknown model');
    expect(
      targetUnavailable({ ...unavailable, providerModelId: 'other' })
    ).toBe(false);
    expect(fidelityLabel()).toBe('Strict');
    expect(fidelityLabel({ mode: 'transformed' })).toBe('Transformed');
  });
});

describe('surfacesFor', () => {
  it('offers the native surface for duplex realtime simulation', () => {
    expect(surfacesFor('realtime')).toEqual(['openai', 'native', 'gemini']);
  });

  it('offers the standard OpenAI surface for embeddings', () => {
    expect(surfacesFor('embeddings')).toEqual(['openai']);
  });
});

describe('gateway tool execution', () => {
  const toolset = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
  const values: RouteEditorValues = {
    slug: 'workflow',
    operations: ['generation'],
    overallTimeoutMs: 120000,
    maxAttempts: 2,
    targets: [
      { providerModelId: 'model', priority: 1, weight: 1, timeoutMs: 60000 }
    ],
    contentPolicyRules: [],
    fidelity: { mode: 'transformed' },
    behavior: {
      ...emptyBehavior(),
      tool_execution: 'gateway',
      responses_mcp_items: true,
      toolsets: [toolset]
    }
  };
  const models: RouteModelOption[] = [
    {
      id: 'model',
      providerId: 'provider',
      providerName: 'Provider',
      upstreamModel: 'model',
      label: 'Provider model',
      capabilities: [],
      lifecycle: null
    }
  ];

  it('keeps gateway execution and toolsets when creating or saving a route', () => {
    expect(buildCreateRouteDraftInput(values, models)).toMatchObject({
      tool_execution: 'gateway',
      responses_mcp_items: true,
      toolsets: [toolset]
    });
    expect(buildReplaceRouteDraftInput(values)).toMatchObject({
      tool_execution: 'gateway',
      responses_mcp_items: true,
      toolsets: [toolset]
    });
  });

  it('requires transformed fidelity for gateway execution', () => {
    expect(
      validateRouteEditor({ ...values, fidelity: { mode: 'strict' } })
    ).toMatch(/transformed/i);
    expect(validateRouteEditor(values)).toBeNull();
  });
});
