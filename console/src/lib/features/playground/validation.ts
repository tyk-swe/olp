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
