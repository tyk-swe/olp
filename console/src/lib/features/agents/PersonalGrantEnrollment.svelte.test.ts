import { expect, it, onTestFinished, vi } from 'vitest';
import { authLifecycle } from '#lib/session/lifecycle.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  mountState,
  mountWithState,
  setFieldValue
} from '#test/mount.js';
import type { MCPServer } from './api';
import GrantsPage from './GrantsPage.svelte';
import { PersonalGrantEnrollment } from './grantEnrollment.svelte';

const configuredServer: MCPServer = {
  id: 'server',
  project_id: 'project',
  name: 'Original',
  transport: 'streamable_http',
  endpoint: 'https://tools.example.test/mcp',
  enabled: true,
  has_credential: false,
  etag: 'before',
  revision_id: 'revision',
  revision: 1,
  certified_at: '2026-01-01T00:00:00Z',
  retired_at: null,
  oauth: {
    client_id: 'client',
    audience: 'audience',
    scopes: ['read'],
    authorization_endpoint: 'https://auth.example.test/authorize',
    token_endpoint: 'https://auth.example.test/token'
  },
  delegation: null,
  unit_price: null,
  tool_prices: {},
  calls_per_minute: 60,
  max_concurrency: 8,
  per_key_calls_per_minute: 60,
  per_key_max_concurrency: 8,
  catalog: { protocol_version: '2025-11-25', digest: 'fixture', tools: [] }
};

function personalEnrollment() {
  return mountState(() => new PersonalGrantEnrollment()).value;
}

it('consumes and clears handoff and callback secrets after their confirmed writes', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      writes(new URL(request.url).pathname, await request.json());
      if (new URL(request.url).pathname.endsWith('/claim'))
        return json({ enrollment: { id: 'grant' }, state: 'pending' });
      return json({ state: 'active' });
    },
    ['read', 'configure']
  );
  const state = personalEnrollment();
  expect(
    state.consumeLocation(
      new URL('https://console.example.test/grants?tab=mine#grant.token')
    )
  ).toBe('/grants?tab=mine');
  await state.claim();
  expect(writes).toHaveBeenCalledWith('/api/v1/mcp-grant-enrollments/claim', {
    enrollment_id: 'grant',
    handoff_token: 'token'
  });
  expect(state.view.phase.kind).toBe('claimed');
  expect(state.view.handoffToken).toBe('');
  expect(
    state.consumeLocation(
      new URL(
        'https://console.example.test/grants?code=secret-code&state=grant.signature'
      )
    )
  ).toBe('/grants');
  await state.completeCallback();
  expect(writes).toHaveBeenCalledWith(
    '/api/v1/mcp-grant-enrollments/grant/complete',
    { code: 'secret-code', state: 'grant.signature' }
  );
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization?.state).toBe('active');
});

it('ignores an obsolete enrollment response after consuming another handoff', async () => {
  const response = Promise.withResolvers<Response>();
  serve(() => response.promise, ['read', 'configure']);
  const state = personalEnrollment();
  const pending = state.begin({ kind: 'server', id: 'server', keyId: 'key' });
  state.consumeLocation(
    new URL('https://console.example.test/grants#new.token')
  );
  response.resolve(
    json({ enrollment_url: 'https://console.example.test/grants#old.token' })
  );
  await pending;
  expect(state.view.enrollmentID).toBe('new');
  expect(state.view.handoffToken).toBe('token');
  expect(state.view.enrollmentURL).toBe('');
  expect(state.view.busy).toBe(false);
});

it('retires a pending authorization and its secrets when the principal changes', async () => {
  const response = Promise.withResolvers<Response>();
  serve(() => response.promise, ['read', 'configure']);
  const state = personalEnrollment();
  state.consumeLocation(
    new URL(
      'https://console.example.test/grants?code=secret&state=grant.signature'
    )
  );
  const pending = state.completeCallback();
  authLifecycle.establishSession({
    user: {
      id: 'other',
      email: 'other@example.test',
      display_name: 'Other',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'other-csrf'
  });
  response.resolve(json({ state: 'active' }));
  await pending;
  expect(state.view.phase.kind).toBe('idle');
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization).toBeUndefined();
});

it('shows the latest personal grant result after saving authorization settings', async () => {
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/mcp-servers/server') return json(configuredServer);
      if (path === '/api/v1/mcp-servers')
        return json({ items: [configuredServer] });
      if (path === '/api/v1/mcp-grant-enrollments/claim')
        return json({ enrollment: { id: 'grant' }, state: 'pending' });
      return json({ items: [] });
    },
    ['read', 'configure', 'keys', 'self']
  );
  const originalURL = window.location.href;
  window.history.replaceState(null, '', window.location.pathname);
  onTestFinished(() => window.history.replaceState(null, '', originalURL));
  const { target } = mountWithState(GrantsPage);
  const button = (label: string) => findButton(target, label);
  await eventually(() =>
    expect(target.querySelector('select option[value="server"]')).not.toBeNull()
  );
  const selection = target
    .querySelector('select option[value="server"]')
    ?.closest<HTMLSelectElement>('select');
  expect(selection).not.toBeNull();
  if (!selection) return;
  setFieldValue(selection, configuredServer.id, 'change');
  await eventually(() =>
    expect(button('Save authorization settings')).toBeDefined()
  );
  button('Save authorization settings')
    ?.closest('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() =>
    expect(target.textContent).toContain(
      'Personal authorization configuration saved'
    )
  );
  window.history.replaceState(null, '', '#grant.token');
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  await eventually(() => expect(button('Claim enrollment')).toBeDefined());
  button('Claim enrollment')?.click();
  await eventually(() =>
    expect(target.textContent).toContain(
      'Enrollment claimed. Authorization is pending'
    )
  );
  expect(target.textContent).not.toContain(
    'Personal authorization configuration saved'
  );
});

it.each([
  {
    button: 'Authorize in browser',
    link: 'Sign in upstream',
    invalidURL: '/authorize',
    validURL: 'https://identity.example.test/authorize?state=example',
    authorization: (authorization_url: string) => ({ authorization_url })
  },
  {
    button: 'Use device authorization',
    link: 'the authorization page',
    invalidURL: 'ftp://identity.example.test/device',
    validURL: 'http://localhost:8081/device',
    authorization: (verification_url: string) => ({
      device: { verification_url, user_code: 'EXAMPLE', interval: 5 }
    })
  }
])(
  'only shows an HTTP(S) link after choosing $button',
  async ({ button, link, invalidURL, validURL, authorization }) => {
    const attempts = vi
      .fn<() => string>()
      .mockReturnValueOnce(invalidURL)
      .mockReturnValue(validURL);
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (path.endsWith('/claim'))
          return json({ enrollment: { id: 'grant' }, state: 'pending' });
        if (path.endsWith('/authorize'))
          return json({ state: 'pending', ...authorization(attempts()) });
        return json({ items: [] });
      },
      ['read', 'configure', 'keys', 'self']
    );
    const originalURL = window.location.href;
    window.history.replaceState(null, '', '#grant.token');
    onTestFinished(() => window.history.replaceState(null, '', originalURL));
    const { target } = mountWithState(GrantsPage);
    const authorizationLink = () =>
      [...target.querySelectorAll('a')].find(
        (anchor) => anchor.textContent?.trim() === link
      );

    await eventually(() =>
      expect(findButton(target, 'Claim enrollment')).toBeDefined()
    );
    findButton(target, 'Claim enrollment')?.click();
    await eventually(() => expect(findButton(target, button)).toBeDefined());
    findButton(target, button)?.click();
    await eventually(() =>
      expect(target.querySelector('[role="alert"]')?.textContent?.trim()).toBe(
        'The personal grant authorization response is invalid'
      )
    );
    expect(authorizationLink()).toBeUndefined();

    findButton(target, button)?.click();
    await eventually(() =>
      expect(authorizationLink()?.getAttribute('href')).toBe(validURL)
    );
  }
);

it('retains the callback while starting and clearing a separate invitation', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      writes(new URL(request.url).pathname, await request.json());
      return new URL(request.url).pathname.endsWith('/complete')
        ? json({ state: 'active' })
        : json({
            enrollment_url: 'https://console.example.test/grants#next.token'
          });
    },
    ['read', 'configure']
  );
  const state = personalEnrollment();
  state.consumeLocation(
    new URL(
      'https://console.example.test/grants?code=secret-code&state=grant.signature'
    )
  );
  await state.begin({ kind: 'server', id: 'server', keyId: 'key' });
  expect(state.view.callbackCode).toBe('secret-code');
  expect(state.view.enrollmentURL).toContain('#next.token');
  state.clearInvitation();
  expect(state.view.enrollmentURL).toBe('');
  await state.completeCallback();
  expect(writes).toHaveBeenCalledWith(
    '/api/v1/mcp-grant-enrollments/grant/complete',
    { code: 'secret-code', state: 'grant.signature' }
  );
  expect(state.view.callbackCode).toBe('');
  expect(state.view.authorization?.state).toBe('active');
});
