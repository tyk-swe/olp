import { isJsonObject, parseJsonObject } from '#lib/editing/json.js';
import { captureResult } from '#lib/editing/result.js';
import {
  nativeObject,
  replaceNative,
  type NativeObject
} from '#lib/nativeJson.js';
import type { PlaygroundOperation } from '#lib/requestTemplates.js';
import type { InspectRoutingInput, PlaygroundRequest } from './api';
import { nativeOperationRequest } from './nativeOperation';
import type {
  Composer,
  Mode,
  PlaygroundPlan,
  PlaygroundSurface
} from './playgroundPlan';
import { fail, parseNativeValue, type Outcome } from './publicGateway';

type PlaygroundTool = NonNullable<PlaygroundRequest['tools']>[number];
type PlaygroundResponseSchema = {
  type: 'json_schema';
  name: string;
  strict: true;
  schema: Record<string, unknown>;
};
type RoutingPreferences = NonNullable<PlaygroundRequest['routing']>;

type SubmitRequestInput = {
  advanced: boolean;
  model: string;
  surface: PlaygroundSurface;
  operation: PlaygroundOperation;
  routing: string;
  rawJson: string;
  input: string;
  mode: Mode;
  temperature: string;
  maxOutputTokens: string;
  toolsJson: string;
  schemaJson: string;
  stream: boolean;
};

type InspectionRequestInput = {
  composer: Composer;
  model: string;
  operation: PlaygroundOperation;
  rawJson: string;
  routing: string;
  stream: boolean;
  simulateKeyId: string;
  simulateSeed: string;
  plan: PlaygroundPlan;
};

function parseJson(value: string): Outcome<unknown> {
  const parsed = captureResult<unknown>(() => JSON.parse(value));
  return parsed.ok
    ? { ok: true, value: parsed.value }
    : fail('Enter valid JSON.');
}

function parseTool(value: unknown): PlaygroundTool | null {
  if (!isJsonObject(value)) return null;
  const { name, description, input_schema } = value;
  if (typeof name !== 'string' || !name.trim()) return null;
  if (description !== undefined && typeof description !== 'string') return null;
  if (!Object.hasOwn(value, 'input_schema')) return null;
  return {
    name: name.trim(),
    ...(description === undefined ? {} : { description }),
    input_schema
  };
}

export function parseTools(
  value: string
): Outcome<PlaygroundTool[] | undefined> {
  if (!value.trim()) return { ok: true, value: undefined };
  const parsed = parseJson(value);
  if (!parsed.ok) return parsed;
  if (!Array.isArray(parsed.value)) {
    return fail(
      'Tools must be an array of name, description, and input_schema objects.'
    );
  }
  const tools = parsed.value.map(parseTool);
  if (tools.some((tool) => tool === null)) {
    return fail(
      'Tools must be an array of name, description, and input_schema objects.'
    );
  }
  return {
    ok: true,
    value: tools.filter((tool): tool is PlaygroundTool => tool !== null)
  };
}

export function parseResponseSchema(
  value: string
): Outcome<PlaygroundResponseSchema | undefined> {
  if (!value.trim()) return { ok: true, value: undefined };
  const parsed = parseJson(value);
  if (!parsed.ok) return parsed;
  if (!isJsonObject(parsed.value)) {
    return fail('The response schema must be a JSON object.');
  }
  return {
    ok: true,
    value: {
      type: 'json_schema',
      name: 'playground_response',
      strict: true,
      schema: parsed.value
    }
  };
}

export function parseTemperature(value: string): Outcome<number | undefined> {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: undefined };
  const temperature = Number(trimmed);
  if (!Number.isFinite(temperature) || temperature < 0 || temperature > 2) {
    return fail('Temperature must be from 0 through 2.');
  }
  return { ok: true, value: temperature };
}

export function parseMaxOutputTokens(
  value: string
): Outcome<number | undefined> {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: undefined };
  const tokens = Number(trimmed);
  if (!Number.isInteger(tokens) || tokens < 1 || tokens > 1_048_576) {
    return fail('Maximum output tokens must be from 1 through 1048576.');
  }
  return { ok: true, value: tokens };
}

export function parseRoutingPreferences(
  value: string
): Outcome<RoutingPreferences> {
  const parsed = parseJsonObject(value);
  if (!parsed) return fail('Routing preferences must be a JSON object.');
  return { ok: true, value: parsed as unknown as RoutingPreferences };
}

function playgroundRequestOperation(
  operation: PlaygroundOperation
): PlaygroundRequest['operation'] {
  switch (operation) {
    case 'generation':
    case 'token_count':
    case 'embeddings':
    case 'moderation':
    case 'rerank':
      return operation;
    default:
      return undefined;
  }
}

function parseAdvancedRequest(
  operation: PlaygroundOperation,
  model: string,
  rawJson: string
): Outcome<Record<string, unknown>> {
  if (operation === 'translation') {
    return { ok: true, value: { model } };
  }
  const raw = parseNativeValue(
    rawJson,
    'The request document must be valid JSON.'
  );
  if (!raw.ok) return raw;
  if (!nativeObject(raw.value)) {
    return fail('The request document must be a JSON object.');
  }
  return { ok: true, value: raw.value };
}

function parseRequestControls(
  input: Pick<
    SubmitRequestInput,
    'mode' | 'temperature' | 'maxOutputTokens' | 'toolsJson' | 'schemaJson'
  >
): Outcome<
  Pick<
    PlaygroundRequest,
    'temperature' | 'max_output_tokens' | 'tools' | 'response_format'
  >
> {
  const temperature = parseTemperature(input.temperature);
  if (!temperature.ok) return temperature;
  const maxOutputTokens = parseMaxOutputTokens(input.maxOutputTokens);
  if (!maxOutputTokens.ok) return maxOutputTokens;
  const tools =
    input.mode === 'tools'
      ? parseTools(input.toolsJson)
      : ({ ok: true, value: undefined } as const);
  if (!tools.ok) return tools;
  const responseFormat =
    input.mode === 'structured'
      ? parseResponseSchema(input.schemaJson)
      : ({ ok: true, value: undefined } as const);
  if (!responseFormat.ok) return responseFormat;
  return {
    ok: true,
    value: {
      temperature: temperature.value,
      max_output_tokens: maxOutputTokens.value,
      tools: tools.value,
      response_format: responseFormat.value
    }
  };
}

function inspectionNativeRequest(
  rawJson: string,
  model: string,
  dialect: string,
  stream: boolean
): Outcome<NativeObject> {
  const request = nativeOperationRequest(rawJson, model, dialect);
  if (!request.ok || dialect !== 'openai-completions') return request;
  const withStream = replaceNative(request.value, ['stream'], stream);
  return nativeObject(withStream)
    ? { ok: true, value: withStream }
    : fail('The native operation request must be a JSON object.');
}

export function buildInspectionInput(
  input: InspectionRequestInput
): Outcome<InspectRoutingInput> {
  const preferences = parseRoutingPreferences(input.routing);
  if (!preferences.ok) return preferences;
  const route = input.model.trim();
  const base: InspectRoutingInput = {
    route,
    operation: input.operation,
    surface: input.plan.inspection.surface,
    mode: input.plan.inspection.mode,
    preferences: preferences.value,
    apiKeyId: input.simulateKeyId || null,
    seed: input.simulateSeed,
    clientContract: input.plan.inspection.clientContract
  };
  if (input.composer !== 'advanced' || input.plan.kind === 'realtime') {
    return { ok: true, value: base };
  }
  const dialect = input.plan.inspection.dialect;
  const request =
    input.plan.inspection.registeredNative || dialect === 'openai-completions'
      ? inspectionNativeRequest(
          input.rawJson,
          route,
          dialect ?? '',
          input.stream
        )
      : parseAdvancedRequest(input.operation, route, input.rawJson);
  if (!request.ok) return request;
  return { ok: true, value: { ...base, request: request.value, dialect } };
}

export function buildPlaygroundRequest(
  input: SubmitRequestInput
): Outcome<PlaygroundRequest> {
  const routing = parseRoutingPreferences(input.routing);
  if (!routing.ok) return routing;
  const model = input.model.trim();
  if (input.advanced) {
    const request = parseAdvancedRequest(input.operation, model, input.rawJson);
    if (!request.ok) return request;
    return {
      ok: true,
      value: {
        routing: routing.value,
        model,
        surface: input.surface,
        operation: playgroundRequestOperation(input.operation),
        request: request.value,
        stream: input.stream ? true : undefined
      }
    };
  }
  if (!input.input.trim()) return fail('Enter a prompt.');
  const controls = parseRequestControls(input);
  if (!controls.ok) return controls;
  return {
    ok: true,
    value: {
      routing: routing.value,
      model,
      input: input.input,
      surface: input.surface,
      stream: input.stream ? true : undefined,
      ...controls.value
    }
  };
}
