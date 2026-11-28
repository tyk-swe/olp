import type { Validation } from '#lib/editing/validation.js';
import { parseRetainedEvidence } from './lifecycle';

export type RetainedField = 'hint' | 'evidence';

export const reconciliationServerFields: Readonly<
  Record<string, RetainedField>
> = { upstream_id: 'hint' };

export const evidenceServerFields: Readonly<Record<string, RetainedField>> = {
  evidence: 'evidence',
  quantities: 'evidence',
  model: 'evidence'
};

export function validateEvidence(
  raw: string
): Validation<string, RetainedField> {
  const parsed = parseRetainedEvidence(raw);
  return parsed.ok
    ? parsed
    : { ok: false, fieldErrors: { evidence: parsed.message } };
}
