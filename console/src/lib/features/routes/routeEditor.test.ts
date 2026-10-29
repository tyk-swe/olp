import { describe, expect, it } from 'vitest';

import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput,
  emptyBehavior,
  modesFor,
  surfacesFor,
  validateRouteEditor,
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

describe('surfacesFor', () => {
  it('offers the native surface for duplex realtime simulation', () => {
    expect(surfacesFor('realtime')).toContain('native');
    expect(surfacesFor('realtime')).toContain('openai');
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
