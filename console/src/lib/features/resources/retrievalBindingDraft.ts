import type { Validation } from '#lib/editing/validation.js';
import type { RetrievalBindingInput } from './api';
import {
  buildRetrievalBindingInput,
  type RetrievalBindingForm
} from './lifecycle';

export type RetrievalBindingField = Exclude<
  keyof RetrievalBindingForm,
  'route' | 'backend' | 'enabled'
>;

export const retrievalBindingServerFields: Readonly<
  Record<string, RetrievalBindingField>
> = {
  name: 'name',
  project_id: 'project',
  target_id: 'target',
  backend_resource: 'resource',
  fields: 'fields',
  key_grants: 'grants'
};

export function validateRetrievalBinding(
  form: RetrievalBindingForm
): Validation<RetrievalBindingInput, RetrievalBindingField> {
  const built = buildRetrievalBindingInput(form);
  if (built.ok) return built;
  const mappings = buildRetrievalBindingInput({ ...form, grants: '' });
  const grants = buildRetrievalBindingInput({ ...form, fields: '{}' });
  return {
    ok: false,
    fieldErrors: {
      fields: mappings.ok ? undefined : mappings.message,
      grants: grants.ok ? undefined : grants.message
    }
  };
}
