import type { CredentialSlot } from '#lib/domain/providers/api.js';
import { captureResult } from '#lib/editing/result.js';
import type { Validation } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import type { ExternalCredentialReference } from './credentials';
import { parseExternalReference } from './externalReference';

export type SlotField =
  | 'name'
  | 'priority'
  | 'weight'
  | 'credential'
  | 'reference'
  | 'models'
  | 'routes'
  | 'keys';

export function newCredentialSlot(): CredentialSlot {
  return {
    id: crypto.randomUUID(),
    name: '',
    enabled: true,
    priority: 0,
    weight: 1,
    credential_version_id: null,
    allowed_models: [],
    allowed_routes: [],
    allowed_api_keys: [],
    requests_per_minute: null,
    tokens_per_minute: null,
    input_tokens_per_minute: null,
    output_tokens_per_minute: null,
    max_concurrency: null
  };
}

export function slotServerFields(
  grant: boolean,
  useExternal: boolean
): Readonly<Record<string, SlotField>> {
  const fields: Readonly<Record<string, SlotField>> = {
    'slot.name': 'name',
    'slot.priority': 'priority',
    'slot.weight': 'weight',
    'slot.allowed_models': 'models',
    'slot.allowed_routes': 'routes',
    'slot.allowed_api_keys': 'keys'
  };
  if (grant) return fields;
  return useExternal
    ? { ...fields, credential_reference: 'reference' }
    : { ...fields, credential: 'credential' };
}

export function validateSlotReference(
  useExternal: boolean,
  value: string
): Validation<ExternalCredentialReference | null, SlotField> {
  if (!useExternal) return { ok: true, value: null };
  const parsed = captureResult(() => parseExternalReference(value));
  return parsed.ok
    ? parsed
    : {
        ok: false,
        fieldErrors: { reference: errorMessage(parsed.error) }
      };
}
