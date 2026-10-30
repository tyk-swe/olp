// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import { json, route, serve } from '$lib/api/test/transport';
import AccountEnrollmentProbe from './test/AccountEnrollmentProbe.svelte';

let component: ReturnType<typeof mount> | undefined;
let client: QueryClient;
let host: HTMLDivElement;

afterEach(async () => {
  if (component) await unmount(component);
  client?.clear();
  host?.remove();
  vi.unstubAllGlobals();
});

it.each([
  {
    name: 'Codex',
    file: 'Codex auth.json',
    profile: 'codex-subscription',
    path: 'codex-import'
  },
  {
    name: 'Claude',
    file: 'Claude .credentials.json',
    profile: 'claude-subscription',
    path: 'claude-import'
  },
  {
    name: 'Antigravity',
    file: 'Antigravity migration JSON',
    profile: 'antigravity-subscription',
    path: 'antigravity-import'
  }
])(
  'imports pasted $name credentials once and clears them before reporting completion',
  async ({ name, file, profile, path: importPath }) => {
    const credential = JSON.stringify(
      profile === 'antigravity-subscription'
        ? {
            type: 'antigravity',
            access_token: 'fixture-access',
            refresh_token: 'fixture-refresh',
            client_id: 'fixture.apps.googleusercontent.com',
            client_secret: 'fixture-secret'
          }
        : profile === 'claude-subscription'
          ? {
              claudeAiOauth: {
                accessToken: 'fixture-access',
                refreshToken: 'fixture-refresh',
                expiresAt: 1900000000000,
                scopes: ['user:profile', 'user:inference']
              }
            }
          : {
              auth_mode: 'chatgpt',
              tokens: {
                access_token: 'fixture-access',
                id_token: 'fixture-id',
                refresh_token: 'fixture-refresh',
                account_id: 'fixture-account'
              }
            }
    );
    const completed = vi.fn();
    const imported = vi.fn();
    serve(async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({
          items: [
            {
              kind: 'plugin',
              auth_modes: [{ mode: 'grant', credential: 'grant' }]
            }
          ]
        });
      if (request.method === 'GET' && path === '/api/v1/providers/provider')
        return json({
          id: 'provider',
          etag: 'draft',
          configuration: {
            kind: 'plugin',
            auth_mode: 'grant',
            profile_id: profile
          }
        });
      imported(request);
      return json(
        {
          provider_id: 'provider',
          etag: 'next',
          credential_id: 'imported',
          credential_version: 1,
          principal: 'verified'
        },
        201
      );
    });
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    host = document.createElement('div');
    document.body.append(host);
    component = mount(AccountEnrollmentProbe, {
      target: host,
      props: { client, onComplete: completed }
    });
    await vi.waitFor(() => {
      flushSync();
      expect(
        host.querySelector(`input[type="password"][aria-label="${file}"]`)
      ).not.toBeNull();
    });
    const input = host.querySelector<HTMLInputElement>(
      `input[type="password"][aria-label="${file}"]`
    )!;
    input.value = '{broken';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    const submit = () =>
      [...host.querySelectorAll('button')].find(
        (button) => button.textContent?.trim() === `Import ${name} credentials`
      )!;
    submit().click();
    flushSync();
    await vi.waitFor(() =>
      expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'JSON'
      )
    );
    expect(imported).not.toHaveBeenCalled();
    input.value = credential;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    submit().click();
    await vi.waitFor(() => {
      flushSync();
      expect(completed).toHaveBeenCalledWith('imported');
      expect(input.value).toBe('');
      expect(host.textContent).toContain('OLP owns refresh');
    });
    const request = imported.mock.calls[0][0] as Request;
    route(request, 'POST', `/api/v1/providers/provider/${importPath}`);
    expect(request.headers.get('If-Match')?.replaceAll('"', '')).toBe('draft');
    expect(await request.text()).toBe(
      JSON.stringify({ credentials: credential })
    );
    expect(host.textContent).not.toContain('fixture-refresh');
  }
);

it.each(['codex', 'claude', 'antigravity'] as const)(
  'clears rejected %s credentials and shows a safe failure without completing enrollment',
  async (adapter) => {
    const credential = '{"tokens":{"refresh_token":"rejected-fixture-secret"}}';
    const completed = vi.fn();
    serve(async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/provider-kinds')
        return json({
          items: [
            {
              kind: 'plugin',
              auth_modes: [{ mode: 'grant', credential: 'grant' }]
            }
          ]
        });
      if (request.method === 'GET')
        return json({
          id: 'provider',
          etag: 'draft',
          configuration: {
            kind: 'plugin',
            auth_mode: 'grant',
            profile_id: `${adapter}-subscription`
          }
        });
      return json(
        {
          type: 'about:blank',
          title: 'Rejected',
          status: 422,
          detail: credential
        },
        422
      );
    });
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    host = document.createElement('div');
    document.body.append(host);
    component = mount(AccountEnrollmentProbe, {
      target: host,
      props: { client, onComplete: completed }
    });
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('input[type="password"]')).not.toBeNull();
    });
    const input = host.querySelector('input[type="password"]')!;
    input.value = credential;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    [...host.querySelectorAll('button')]
      .find(
        (button) =>
          button.textContent?.trim() ===
          `Import ${adapter === 'claude' ? 'Claude' : adapter === 'antigravity' ? 'Antigravity' : 'Codex'} credentials`
      )!
      .click();
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'Could not import'
      );
    });
    expect(input.value).toBe('');
    expect(host.textContent).not.toContain('rejected-fixture-secret');
    expect(completed).not.toHaveBeenCalled();
  }
);
