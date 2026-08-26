import type { PlaygroundRequest } from '$lib/api/playground';

type PlaygroundTool = NonNullable<PlaygroundRequest['tools']>[number];

function parseJson(value: string): unknown {
  try {
    return JSON.parse(value) as unknown;
  } catch {
    throw new Error('Enter valid JSON.');
  }
}

export function parseTools(value: string) {
  if (!value.trim()) return undefined;
  const parsed = parseJson(value);
  if (
    !Array.isArray(parsed) ||
    !parsed.every(
      (tool) =>
        typeof tool === 'object' &&
        tool !== null &&
        typeof Reflect.get(tool, 'name') === 'string' &&
        Reflect.get(tool, 'name').trim() !== '' &&
        (Reflect.get(tool, 'description') === undefined ||
          typeof Reflect.get(tool, 'description') === 'string') &&
        Object.hasOwn(tool, 'input_schema')
    )
  ) {
    throw new Error(
      'Tools must be an array of name, description, and input_schema objects.'
    );
  }
  return parsed.map((tool) => {
    const source = tool as Record<string, unknown>;
    return {
      name: (source.name as string).trim(),
      ...(source.description === undefined
        ? {}
        : { description: source.description as string }),
      input_schema: source.input_schema
    } satisfies PlaygroundTool;
  });
}

export function parseResponseSchema(value: string):
  | {
      type: 'json_schema';
      name: string;
      strict: true;
      schema: Record<string, unknown>;
    }
  | undefined {
  if (!value.trim()) return undefined;
  const schema = parseJson(value);
  if (typeof schema !== 'object' || schema === null || Array.isArray(schema)) {
    throw new Error('The response schema must be a JSON object.');
  }
  return {
    type: 'json_schema',
    name: 'playground_response',
    strict: true,
    schema: schema as Record<string, unknown>
  };
}

/**
 * Sampling bounds mirror the backend so an out-of-range value is named at the
 * field instead of coming back as a 422 with no context.
 */
export function parseTemperature(value: string): number | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  const temperature = Number(trimmed);
  if (!Number.isFinite(temperature) || temperature < 0 || temperature > 2) {
    throw new Error('Temperature must be from 0 through 2.');
  }
  return temperature;
}

export function parseMaxOutputTokens(value: string): number | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  const tokens = Number(trimmed);
  if (!Number.isInteger(tokens) || tokens < 1 || tokens > 1_000_000) {
    throw new Error('Maximum output tokens must be from 1 through 1000000.');
  }
  return tokens;
}
