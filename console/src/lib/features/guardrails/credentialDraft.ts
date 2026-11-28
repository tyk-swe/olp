import { captureResult } from '#lib/editing/result.js';
import type { Validation } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import { nativeObject, parseManagementJSON } from '#lib/nativeJson.js';
import type { createCredential } from './api';

type CredentialDraft = { project: string; name: string; secret: string };

const objectRequired = 'Enter a credential JSON object';

export function validateCredential(
  draft: CredentialDraft
): Validation<Parameters<typeof createCredential>[0], 'secret'> {
  const parsed = captureResult(() => parseManagementJSON(draft.secret));
  if (parsed.ok && nativeObject(parsed.value))
    return {
      ok: true,
      value: {
        name: draft.name,
        project_id: draft.project,
        credential: parsed.value
      }
    };
  const secret = parsed.ok
    ? objectRequired
    : errorMessage(parsed.error, objectRequired);
  return { ok: false, fieldErrors: { secret } };
}
