import * as v from 'valibot';

const ToolsSchema = v.array(v.object({
  name: v.pipe(v.string(), v.trim(), v.minLength(1)),
  description: v.optional(v.string()),
  input_schema: v.unknown()
}));
const JsonSchema = v.record(v.string(), v.unknown());

function parseJson(value: string): unknown {
  try {
    return JSON.parse(value) as unknown;
  } catch {
    throw new Error('Enter valid JSON.');
  }
}

export function parseTools(value: string) {
  if (!value.trim()) return undefined;
  const result = v.safeParse(ToolsSchema, parseJson(value));
  if (!result.success) throw new Error('Tools must be an array of name, description, and input_schema objects.');
  return result.output;
}

export function parseResponseSchema(value: string): {
  type: 'json_schema';
  name: string;
  strict: true;
  schema: Record<string, unknown>;
} | undefined {
  if (!value.trim()) return undefined;
  const result = v.safeParse(JsonSchema, parseJson(value));
  if (!result.success) throw new Error('The response schema must be a JSON object.');
  return {
    type: 'json_schema',
    name: 'playground_response',
    strict: true,
    schema: result.output
  };
}
