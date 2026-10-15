import type { components } from './schema';
export type ApplyRequest = components['schemas']['GuardrailApplyInput'];
export type ApplyResult = components['schemas']['GuardrailApplyResult'];

/** Evaluate a pinned policy; content is sent only in this request body. */
export async function applyGuardrails(
  baseURL: string,
  apiKey: string,
  input: ApplyRequest,
  options: { signal?: AbortSignal; fetch?: typeof fetch } = {}
): Promise<ApplyResult> {
  const response = await (options.fetch ?? globalThis.fetch)(
    new URL('/v1/guardrails/apply', baseURL),
    {
      method: 'POST',
      headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
      signal: options.signal,
      redirect: 'error',
      cache: 'no-store'
    }
  );
  if (!response.ok) throw new Error(`Guardrail evaluation failed (${response.status})`);
  return response.json() as Promise<ApplyResult>;
}
