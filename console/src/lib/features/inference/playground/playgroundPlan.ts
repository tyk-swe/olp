import { inspectionDialects } from '#lib/features/routes/index.js';
import type { InspectRoutingInput, PlaygroundOperation } from './api';
import {
  nativeDialects,
  publicNativeOperations,
  type NativeOperation
} from './nativeOperation';

export type Mode = 'text' | 'tools' | 'structured';
export type Composer = 'basic' | 'advanced';
export type PlaygroundSurface = 'openai' | 'anthropic' | 'gemini';

type SimulationDialect = NonNullable<InspectRoutingInput['dialect']>;

type PlaygroundPlanInput = {
  composer: Composer;
  operation: PlaygroundOperation;
  surface: PlaygroundSurface;
  strict: boolean;
  template: string;
  nativeDialect: string;
  inspectDialect: string;
  stream: boolean;
  operationKnown: boolean;
};

type PlaygroundClient =
  | { kind: 'composer'; advanced: boolean }
  | { kind: 'native'; operation: NativeOperation; dialect: string }
  | { kind: 'strict' }
  | { kind: 'audio' }
  | { kind: 'realtime' }
  | { kind: 'unavailable' };

export type PlaygroundPlan = PlaygroundClient & {
  activeOperation: PlaygroundOperation;
  submissionError: string | null;
  inspection: {
    surface: InspectRoutingInput['surface'];
    mode: InspectRoutingInput['mode'];
    dialect: SimulationDialect | undefined;
    registeredNative: boolean;
    clientContract: string | undefined;
    dialects: readonly string[];
  };
};

const simulationDialects = [
  'openai-chat',
  'openai-responses',
  'anthropic-messages',
  'gemini-generate-content',
  'openai-input-tokens',
  'anthropic-count-tokens',
  'gemini-count-tokens',
  'openai-embeddings',
  'openai-moderation',
  'rerank',
  'voyage-embeddings',
  'tei-embeddings',
  'tei-sparse-embeddings',
  'tei-multivector-embeddings',
  'gemini-embeddings',
  'gemini-batch-embeddings',
  'vertex-embeddings',
  'bedrock-embeddings',
  'voyage-rerank',
  'tei-rerank',
  'tei-classification',
  'tei-scoring',
  'bedrock-count-tokens',
  'tei-tokenize',
  'openai-completions',
  'openai-decisions',
  'systemone',
  'gemini-decisions',
  'mistral-ocr',
  'azure-document-intelligence-ocr',
  'vertex-document-processing-ocr',
  'perplexity-search',
  'tavily-search',
  'exa-search',
  'parallel-search',
  'brave-search',
  'cohere-embed-v2',
  'cohere-rerank-v2'
] as const satisfies readonly SimulationDialect[];

function toSimulationDialect(
  value: string | undefined
): SimulationDialect | undefined {
  return simulationDialects.find((candidate) => candidate === value);
}

function nativeOperationFor(
  operation: PlaygroundOperation
): NativeOperation | null {
  switch (operation) {
    case 'decision':
    case 'embeddings':
    case 'rerank':
    case 'moderation':
    case 'classification':
    case 'scoring':
    case 'token_count':
    case 'ocr':
    case 'search':
      return operation;
    case 'generation':
    case 'translation':
    case 'realtime':
      return null;
  }
}

function resolveInspectionSurface(
  operation: PlaygroundOperation,
  surface: PlaygroundSurface,
  nativeDialect: string,
  registeredNative: boolean
): InspectRoutingInput['surface'] {
  if (!registeredNative) return surface;
  if (!publicNativeOperations.has(operation)) return 'native';
  return (
    (['openai', 'gemini'] as const).find((candidate) =>
      inspectionDialects(operation, candidate).includes(nativeDialect)
    ) ?? 'native'
  );
}

function resolveInspectionMode(
  operation: PlaygroundOperation,
  registeredNative: boolean,
  stream: boolean
): InspectRoutingInput['mode'] {
  if (operation === 'realtime') return 'realtime';
  if (registeredNative) return 'unary';
  return stream ? 'streaming' : 'unary';
}

function resolveClient(
  input: PlaygroundPlanInput,
  operation: PlaygroundOperation,
  nativeDialect: string,
  hasNativeDialects: boolean
): PlaygroundClient {
  if (operation === 'realtime') return { kind: 'realtime' };
  if (operation === 'translation') return { kind: 'audio' };
  const advanced = input.composer === 'advanced';
  if (!input.strict && !publicNativeOperations.has(operation)) {
    return { kind: 'composer', advanced };
  }
  if (advanced && operation === 'generation' && input.surface === 'openai') {
    if (input.template === 'legacy-completions') {
      return {
        kind: 'native',
        operation: 'completions',
        dialect: 'openai-completions'
      };
    }
    return { kind: 'strict' };
  }
  const nativeOp = nativeOperationFor(operation);
  if (advanced && hasNativeDialects && nativeOp) {
    return { kind: 'native', operation: nativeOp, dialect: nativeDialect };
  }
  return { kind: 'unavailable' };
}

function resolveSubmissionError(
  input: PlaygroundPlanInput,
  operation: PlaygroundOperation,
  client: PlaygroundClient
): string | null {
  if (input.composer === 'advanced' && !input.operationKnown) {
    return `The route does not offer the ${operation} operation.`;
  }
  if (input.strict && operation !== 'realtime') {
    return 'Use the qualified public client below for this strict route.';
  }
  if (client.kind === 'audio') {
    return 'Use the audio upload form below.';
  }
  if (client.kind === 'realtime') {
    return 'Use the local realtime event viewer below; it does not open a provider session.';
  }
  if (
    client.kind !== 'composer' ||
    operation === 'classification' ||
    operation === 'scoring'
  ) {
    return 'Use the public native client below with a route certified for this operation.';
  }
  return null;
}

export function resolvePlaygroundPlan(
  input: PlaygroundPlanInput
): PlaygroundPlan {
  const advanced = input.composer === 'advanced';
  const operation = advanced ? input.operation : 'generation';
  const options = nativeDialects(operation);
  const nativeDialect = options.includes(input.nativeDialect)
    ? input.nativeDialect
    : (options[0] ?? '');
  const registeredNative =
    advanced &&
    (input.strict || publicNativeOperations.has(operation)) &&
    options.length > 0;
  const inspectOptions = inspectionDialects(operation, input.surface);
  const selectedDialect = registeredNative
    ? nativeDialect
    : advanced && inspectOptions.includes(input.inspectDialect)
      ? input.inspectDialect
      : undefined;
  const client = resolveClient(
    input,
    operation,
    nativeDialect,
    options.length > 0
  );
  return {
    ...client,
    activeOperation: operation,
    submissionError: resolveSubmissionError(input, operation, client),
    inspection: {
      surface: resolveInspectionSurface(
        operation,
        input.surface,
        nativeDialect,
        registeredNative
      ),
      mode: resolveInspectionMode(operation, registeredNative, input.stream),
      dialect: toSimulationDialect(selectedDialect),
      registeredNative,
      clientContract:
        registeredNative && operation === 'embeddings'
          ? 'raw-vector-storage/1'
          : undefined,
      dialects: registeredNative ? options : inspectOptions
    }
  };
}
