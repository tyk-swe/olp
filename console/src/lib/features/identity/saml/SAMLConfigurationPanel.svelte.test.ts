import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import SAMLConfigurationPanel from './SAMLConfigurationPanel.svelte';

function refusal(field: string, message: string) {
  return json(
    {
      title: 'Validation failed',
      status: 422,
      errors: { [field]: [{ code: 'invalid', message }] }
    },
    422
  );
}

it('reports each refused value at the input that holds it', async () => {
  const saves = vi.fn();
  serve(
    (request) => {
      if (request.method === 'GET')
        return json({ title: 'Not found', status: 404 }, 404);
      if (request.method === 'POST')
        return refusal('url', 'Use a safe identity metadata URL.');
      saves();
      return refusal('metadata_xml', 'Import one SAML 2.0 IdP descriptor.');
    },
    ['access', 'access_read']
  );
  const { target } = mountWithState(
    SAMLConfigurationPanel,
    {},
    { attach: true }
  );
  const control = (label: string) =>
    [...target.querySelectorAll('label')].find(
      (item) => item.textContent?.trim() === label
    )!.control as HTMLInputElement;
  await eventually(() =>
    expect(findButton(target, 'Save SAML configuration')).toBeDefined()
  );
  const url = control('Metadata URL');
  const metadata = control('Identity provider metadata XML');
  const mappings = control('Email role mappings (JSON)');

  setFieldValue(url, 'https://id.example.test/metadata');
  findButton(target, 'Import metadata')!.click();
  await eventually(() => expect(document.activeElement).toBe(url));
  expect(reportedError(url)).toBe('Use a safe identity metadata URL.');

  setFieldValue(mappings, '[');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(mappings));
  expect(reportedError(mappings)).toBeTruthy();
  expect(reportedError(url)).toBeUndefined();
  expect(saves).not.toHaveBeenCalled();

  setFieldValue(mappings, '[]');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(metadata));
  expect(reportedError(metadata)).toBe('Import one SAML 2.0 IdP descriptor.');
  expect(reportedError(mappings)).toBeUndefined();
  expect(target.querySelector('[role="alert"]')).toBeNull();
});
