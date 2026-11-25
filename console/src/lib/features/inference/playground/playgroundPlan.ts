import { inspectionDialects } from '#lib/features/routes/index.js';
import type { InspectRoutingInput, PlaygroundOperation } from './api';
import {
  nativeDialects,
  publicNativeOperations,
  type NativeOperation
} from './nativeOperation';

export type Composer = 'basic' | 'advanced';

type PlaygroundPlanInput = {
  composer: Composer;
  operation: PlaygroundOperation;
  surface: 'openai' | 'anthropic' | 'gemini';
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
    dialect: InspectRoutingInput['dialect'] | undefined;
    registeredNative: boolean;
    clientContract: string | undefined;
    dialects: readonly string[];
  };
};

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
  const dialect = registeredNative
    ? nativeDialect
    : advanced &&
        (inspectOptions as readonly string[]).includes(input.inspectDialect)
      ? input.inspectDialect
      : undefined;
  const inspectionSurface = !registeredNative
    ? input.surface
    : publicNativeOperations.has(operation)
      ? ((['openai', 'gemini'] as const).find((surface) =>
          inspectionDialects(operation, surface).includes(nativeDialect)
        ) ?? 'native')
      : 'native';
  const client: PlaygroundClient =
    operation === 'realtime'
      ? { kind: 'realtime' }
      : operation === 'translation'
        ? { kind: 'audio' }
        : input.strict || publicNativeOperations.has(operation)
          ? advanced && operation === 'generation' && input.surface === 'openai'
            ? input.template === 'legacy-completions'
              ? {
                  kind: 'native',
                  operation: 'completions',
                  dialect: 'openai-completions'
                }
              : { kind: 'strict' }
            : advanced && options.length > 0
              ? {
                  kind: 'native',
                  operation: operation as NativeOperation,
                  dialect: nativeDialect
                }
              : { kind: 'unavailable' }
          : { kind: 'composer', advanced };
  const submissionError =
    advanced && !input.operationKnown
      ? `The route does not offer the ${operation} operation.`
      : input.strict && operation !== 'realtime'
        ? 'Use the qualified public client below for this strict route.'
        : client.kind === 'audio'
          ? 'Use the audio upload form below.'
          : client.kind === 'realtime'
            ? 'Use the local realtime event viewer below; it does not open a provider session.'
            : client.kind !== 'composer' ||
                operation === 'classification' ||
                operation === 'scoring'
              ? 'Use the public native client below with a route certified for this operation.'
              : null;
  return {
    ...client,
    activeOperation: operation,
    submissionError,
    inspection: {
      surface: inspectionSurface,
      mode:
        operation === 'realtime'
          ? 'realtime'
          : registeredNative
            ? 'unary'
            : input.stream
              ? 'streaming'
              : 'unary',
      dialect: dialect as InspectRoutingInput['dialect'] | undefined,
      registeredNative,
      clientContract:
        registeredNative && operation === 'embeddings'
          ? 'raw-vector-storage/1'
          : undefined,
      dialects: registeredNative ? options : inspectOptions
    }
  };
}
