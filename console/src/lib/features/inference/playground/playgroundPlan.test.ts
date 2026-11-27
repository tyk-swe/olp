import { describe, expect, it } from 'vitest';
import { resolvePlaygroundPlan } from './playgroundPlan';

describe('playground execution plans', () => {
  const input = {
    composer: 'advanced' as const,
    operation: 'embeddings' as const,
    surface: 'openai' as const,
    strict: true,
    template: '',
    nativeDialect: 'openai-embeddings',
    inspectDialect: '',
    stream: true,
    operationKnown: true
  };

  it('keeps a strict native embedding client and its dry run on the same unary contract', () => {
    const plan = resolvePlaygroundPlan(input);
    expect(plan).toMatchObject({
      kind: 'native',
      operation: 'embeddings',
      dialect: 'openai-embeddings',
      inspection: {
        mode: 'unary',
        surface: 'native',
        dialect: 'openai-embeddings',
        registeredNative: true,
        clientContract: 'raw-vector-storage/1'
      }
    });
    expect(plan.submissionError).toBeTruthy();
  });

  it('inspects Gemini Decisions on the surface used by its public client', () => {
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'decision',
        nativeDialect: 'gemini-decisions'
      })
    ).toMatchObject({
      kind: 'native',
      dialect: 'gemini-decisions',
      inspection: {
        mode: 'unary',
        surface: 'gemini',
        dialect: 'gemini-decisions'
      }
    });
  });

  it('uses the basic generation composer after leaving a dedicated advanced operation', () => {
    const plan = resolvePlaygroundPlan({
      ...input,
      composer: 'basic',
      strict: false
    });
    expect(plan.kind).toBe('composer');
    expect(plan.activeOperation).toBe('generation');
    expect(plan.submissionError).toBeNull();
    expect(plan.inspection.registeredNative).toBe(false);
  });

  it.each(['classification', 'scoring'] as const)(
    'preserves the client visibility and submission restriction for non-strict %s',
    (operation) => {
      const plan = resolvePlaygroundPlan({
        ...input,
        operation,
        strict: false
      });
      expect(plan.kind).toBe('composer');
      expect(plan.inspection.registeredNative).toBe(false);
      expect(plan.submissionError).toContain('Use the public native client');
    }
  );

  it('keeps strict realtime requests local without opening a native provider session', () => {
    const plan = resolvePlaygroundPlan({ ...input, operation: 'realtime' });
    expect(plan.kind).toBe('realtime');
    expect(plan.inspection.mode).toBe('realtime');
    expect(plan.submissionError).toContain('local realtime event viewer');
  });

  it('routes strict OpenAI generation to the strict tool client or legacy completions client', () => {
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'generation',
        template: 'openai-chat'
      })
    ).toMatchObject({
      kind: 'strict',
      submissionError:
        'Use the qualified public client below for this strict route.'
    });
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'generation',
        template: 'legacy-completions'
      })
    ).toMatchObject({
      kind: 'native',
      operation: 'completions',
      dialect: 'openai-completions'
    });
  });

  it('marks strict basic or non-OpenAI generation unavailable and reports unknown advanced operations', () => {
    expect(
      resolvePlaygroundPlan({ ...input, composer: 'basic', strict: true })
    ).toMatchObject({
      kind: 'unavailable',
      activeOperation: 'generation'
    });
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'generation',
        surface: 'anthropic'
      })
    ).toMatchObject({ kind: 'unavailable' });
    expect(
      resolvePlaygroundPlan({ ...input, operationKnown: false })
    ).toMatchObject({
      submissionError: 'The route does not offer the embeddings operation.'
    });
  });

  it('routes audio translation to the upload form', () => {
    expect(
      resolvePlaygroundPlan({
        ...input,
        operation: 'translation',
        strict: false
      })
    ).toMatchObject({
      kind: 'audio',
      submissionError: 'Use the audio upload form below.'
    });
  });
});
