import { describe, expect, it } from 'vitest';
import { inspectionDialects } from '$lib/features/routes/inspectionDialects';
import {
  nativeDialects,
  nativeOperationRequest,
  publicNativeOperations
} from './nativeOperation';
import { playgroundTemplates, templateFor } from './templates';

describe('playground examples', () => {
  it('offers uniquely named examples that can be selected by key', () => {
    const keys = playgroundTemplates.map((template) => template.key);

    expect(keys.length).toBeGreaterThan(0);
    expect(new Set(keys).size).toBe(keys.length);
    for (const template of playgroundTemplates) {
      expect(template.key.trim()).not.toBe('');
      expect(template.label.trim()).not.toBe('');
      expect(templateFor(template.key)).toEqual(template);
    }
    expect(templateFor('unknown-example')).toBeUndefined();
  });

  it('uses registered dialects and binds model-bearing native examples to the chosen route', () => {
    for (const template of playgroundTemplates) {
      if (publicNativeOperations.has(template.operation)) {
        expect(template.nativeDialect).toBeTruthy();
      }
      if (!template.nativeDialect) continue;

      expect(nativeDialects(template.operation)).toContain(
        template.nativeDialect
      );
      const request = nativeOperationRequest(
        JSON.stringify(template.request),
        'chosen-route',
        template.nativeDialect
      );
      if ('model' in template.request)
        expect(request.model).toBe('chosen-route');
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
