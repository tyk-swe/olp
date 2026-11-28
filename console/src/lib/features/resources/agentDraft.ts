import { captureResult } from '#lib/editing/result.js';
import type { Validation } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import { agentSubmissionFingerprint } from './lifecycle';

export function validateAgentDefinition(
  route: string,
  definition: string
): Validation<string, 'definition'> {
  const parsed = captureResult(() =>
    agentSubmissionFingerprint(route, definition)
  );
  return parsed.ok
    ? parsed
    : {
        ok: false,
        fieldErrors: {
          definition: errorMessage(
            parsed.error,
            'The resource request could not be completed.'
          )
        }
      };
}
