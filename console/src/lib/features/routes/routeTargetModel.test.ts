import { describe, expect, it } from 'vitest';

import { fidelityLabel } from './routeFidelity';
import {
  certifiedCapabilities,
  eligibleTargetTuples,
  missingTargetOperations,
  routeEligibilityWarnings,
  storedTargetLabel,
  targetUnavailable,
  type EditableTarget,
  type RouteModelOption
} from './routeTargetModel';

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
