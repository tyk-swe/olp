import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import OidcConfigurationPanel from './OidcConfigurationPanel.svelte';

it('reports each refused setting at the input that holds it', async () => {
  const saves = vi.fn();
  serve(
    (request) => {
      if (request.method === 'GET')
        return json({ title: 'Not found', status: 404 }, 404);
      saves();
      return json(
        {
          title: 'Validation failed',
          status: 422,
          errors: {
            discovery_url: [
              { code: 'invalid', message: 'Invalid OIDC discovery document.' }
            ]
          }
        },
        422
      );
    },
    ['read', 'access', 'access_read']
  );
  const { target } = mountWithState(
    OidcConfigurationPanel,
    {},
    { attach: true }
  );
  const issuer = await eventually(() => {
    const input = findControl(target, 'Expected issuer');
    expect(input.disabled).toBe(false);
    return input;
  });
  const discovery = findControl(target, 'Discovery URL');
  const clientId = findControl(target, 'Client ID');
  const mappings = target.querySelector('textarea')!;
  setFieldValue(issuer, 'https://id.example.test');
  setFieldValue(discovery, 'id.example.test');
  setFieldValue(mappings, 'owner@example.test');

  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(discovery));
  expect(reportedError(discovery)).toBe('Enter the discovery URL.');
  expect(reportedError(clientId)).toBe('Enter the client ID.');
  expect(reportedError(mappings)).toBe(
    'Mapping “owner@example.test” must use claim-value=role.'
  );
  expect(reportedError(issuer)).toBeUndefined();
  expect(saves).not.toHaveBeenCalled();

  setFieldValue(discovery, 'https://id.example.test/discovery');
  setFieldValue(clientId, 'console');
  setFieldValue(mappings, '');
  submitForm(target);
  await eventually(() =>
    expect(reportedError(discovery)).toBe('Invalid OIDC discovery document.')
  );
  expect(reportedError(mappings)).toBeUndefined();
  expect(target.querySelector('[role="alert"]')).toBeNull();
});

it('holds the link button while it redirects and reports a refused link', async () => {
  let refuse: (response: Response) => void = () => {};
  serve(
    (request) => {
      const { pathname } = new URL(request.url);
      if (pathname === '/api/v1/oidc/configuration')
        return json({
          client_id: 'console',
          discovery_url: 'https://id.example.test/discovery',
          email_claim: 'email',
          email_role_mappings: [],
          enabled: true,
          etag: 'etag',
          group_role_mappings: [],
          groups_claim: 'groups',
          has_client_secret: true,
          id: 'oidc',
          issuer: 'https://id.example.test',
          scopes: ['openid']
        });
      if (pathname === '/api/v1/oidc/identities')
        return json({
          has_local_password: true,
          items: [],
          linking_available: true
        });
      if (pathname === '/api/v1/profile/reauthenticate') return json({});
      return new Promise<Response>((resolve) => (refuse = resolve));
    },
    ['read', 'access', 'access_read']
  );
  const { target } = mountWithState(
    OidcConfigurationPanel,
    {},
    { attach: true }
  );
  const link = await eventually(() => {
    const button = findButton(target, 'Link my identity')!;
    expect(button.disabled).toBe(false);
    return button;
  });

  link.click();
  const password = await eventually(() =>
    findControl(target, 'Current password')
  );
  setFieldValue(password, 'a long browser test password');
  password.form!.requestSubmit();

  await eventually(() =>
    expect(findButton(target, 'Redirecting…')?.disabled).toBe(true)
  );
  expect(target.querySelector('dialog')).toBeNull();

  refuse(
    json(
      {
        title: 'Conflict',
        status: 409,
        detail: 'This identity is already linked.'
      },
      409
    )
  );
  await eventually(() =>
    expect(target.querySelector('[role="alert"]')?.textContent).toContain(
      'This identity is already linked.'
    )
  );
  expect(findButton(target, 'Link my identity')?.disabled).toBe(false);
});
