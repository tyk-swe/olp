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

it('imports pasted Codex credentials once and clears them before reporting completion', async () => {
  const credential = JSON.stringify({
    auth_mode: 'chatgpt',
    tokens: {
      access_token: 'fixture-access',
      id_token: 'fixture-id',
      refresh_token: 'fixture-refresh',
      account_id: 'fixture-account'
    }
  });
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
          profile_id: 'codex-subscription'
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
      host.querySelector('textarea[aria-label="Codex auth.json"]')
    ).not.toBeNull();
  });
  const input = host.querySelector<HTMLTextAreaElement>(
    'textarea[aria-label="Codex auth.json"]'
  )!;
  input.value = '{broken';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const submit = () =>
    [...host.querySelectorAll('button')].find(
      (button) => button.textContent?.trim() === 'Import Codex credentials'
    )!;
  submit().click();
  flushSync();
  await vi.waitFor(() =>
    expect(host.querySelector('[role="alert"]')?.textContent).toContain('JSON')
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
  expect(imported).toHaveBeenCalledTimes(1);
  const request = imported.mock.calls[0][0] as Request;
  route(request, 'POST', '/api/v1/providers/provider/codex-import');
  expect(request.headers.get('If-Match')?.replaceAll('"', '')).toBe('draft');
  expect(await request.text()).toBe(
    JSON.stringify({ credentials: credential })
  );
  expect(host.textContent).not.toContain('fixture-refresh');
});

it('clears rejected credentials and shows a safe failure without completing enrollment', async () => {
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
          profile_id: 'codex-subscription'
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
    expect(host.querySelector('textarea')).not.toBeNull();
  });
  const input = host.querySelector('textarea')!;
  input.value = credential;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  [...host.querySelectorAll('button')]
    .find(
      (button) => button.textContent?.trim() === 'Import Codex credentials'
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
});
