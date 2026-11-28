import { describe, expect, it } from 'vitest';

import { emptyBehavior } from './routeBehavior';
import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput
} from './routeDraftInput';
import type { RouteEditorValues } from './routeEditor';
import type { EditableTarget, RouteModelOption } from './routeTargetModel';

describe('route draft requests', () => {
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

  it('keeps gateway execution and toolsets when creating or saving a route', () => {
    const toolset = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
    const values: RouteEditorValues = {
      ...route,
      behavior: {
        ...emptyBehavior(),
        tool_execution: 'gateway',
        responses_mcp_items: true,
        toolsets: [toolset]
      }
    };
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
});
