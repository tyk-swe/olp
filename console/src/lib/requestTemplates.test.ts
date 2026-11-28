import { describe, expect, it } from 'vitest';
import {
  nativeDialects,
  nativeOperationRequest,
  publicNativeOperations
} from '#lib/features/inference/playground/nativeOperation.js';
import { inspectionDialects } from '#lib/features/routes/inspectionDialects.js';
import { playgroundTemplates } from './requestTemplates.js';

describe('playground examples', () => {
  it('uses registered dialects and binds model-bearing native examples to the chosen route', () => {
    for (const template of playgroundTemplates) {
      if (publicNativeOperations.has(template.operation)) {
        expect(template.nativeDialect).toBeTruthy();
      }
      if (!template.nativeDialect) continue;

      expect(nativeDialects(template.operation)).toContain(
        template.nativeDialect
      );
      const outcome = nativeOperationRequest(
        JSON.stringify(template.request),
        'chosen-route',
        template.nativeDialect
      );
      expect(outcome.ok).toBe(true);
      if (outcome.ok && 'model' in template.request) {
        expect(outcome.value.model).toBe('chosen-route');
      }
    }
  });

  it('uses inspection dialects available for the selected operation and surface', () => {
    for (const template of playgroundTemplates) {
      if (!template.inspectDialect) continue;
      expect(
        inspectionDialects(template.operation, template.surface ?? 'openai')
      ).toContain(template.inspectDialect);
    }
  });
});
