import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import type { Provider } from '#lib/domain/providers/api.js';
import ProviderCredentialPool from './ProviderCredentialPool.svelte';

const provider: Provider = {
  id: 'provider',
  etag: 'original',
  name: 'Service',
  configuration: { kind: 'openai', auth_mode: 'api_key' },
  capability_count: 0,
  certified_capability_count: 0,
  enabled_model_count: 0,
  model_count: 0,
  connector_ready: false,
  pending_activation: true,
  project_id: null,
  project_name: null,
  state: 'draft',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

it('reports a refused credential slot at the input that holds the problem', async () => {
  const writes = vi.fn();
  serve(
    (request) => {
      if (request.method === 'GET')
        return json({ items: [], etag: 'pool', health: {} });
      writes();
      return json(
        {
          title: 'Validation failed',
          status: 422,
          errors: {
            'slot.name': [{ code: 'invalid', message: 'This name is taken.' }],
            credential_reference: [
              { code: 'invalid', message: 'This version is not approved.' }
            ]
          }
        },
        422
      );
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(
    ProviderCredentialPool,
    { provider, canManage: true, onChanged: () => {} },
    { attach: true }
  );
  const add = await eventually(() => {
    const found = findButton(target, 'Add credential');
    expect(found).toHaveProperty('disabled', false);
    return found!;
  });
  add.click();
  flushSync();
  const name = target.querySelector<HTMLInputElement>('input[required]')!;
  setFieldValue(name, 'primary');
  target.querySelector<HTMLInputElement>('input[type="checkbox"]')!.click();
  flushSync();
  const reference = target.querySelector('textarea')!;

  setFieldValue(reference, '{}');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(reference));
  expect(reportedError(reference)).toBe(
    'Choose a supported store, secret ID and immutable version.'
  );
  expect(writes).not.toHaveBeenCalled();

  setFieldValue(
    reference,
    '{"store":"gcp","secret_id":"projects/fixture/secrets/provider","version":"1"}'
  );
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(name));
  expect(reportedError(name)).toBe('This name is taken.');
  expect(reportedError(reference)).toBe('This version is not approved.');
  expect(target.querySelector('[role="alert"]')).toBeNull();
});
