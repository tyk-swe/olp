// @vitest-environment jsdom
import { mount, unmount, flushSync } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, vi } from 'vitest';
import WorkloadProbe from './test/WorkloadProbe.svelte';
import { saveIssuer } from './api';
vi.mock('../session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true })
}));
vi.mock('./api', () => ({
  listIssuers: vi.fn(async () => []),
  saveIssuer: vi.fn(async () => {
    throw new Error('Response unavailable');
  }),
  issuerInput: vi.fn()
}));
it('keeps the submitted configuration and replay identity on a lost create response', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const target = document.createElement('div');
  document.body.append(target);
  const view = mount(WorkloadProbe, { target, props: { client } });
  const click = (label: string) => {
    const button = [...target.querySelectorAll('button')].find(
      (button) => button.textContent?.trim() === label
    );
    expect(button).toBeTruthy();
    button!.click();
    flushSync();
  };
  const fill = (id: string, value: string) => {
    const input = target.querySelector<HTMLInputElement | HTMLTextAreaElement>(
      `#${id}`
    )!;
    input.value = value;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
  };
  try {
    flushSync();
    click('Add workload issuer');
    fill('issuer-name', 'CI');
    fill('issuer-url', 'https://issuer.example');
    fill('issuer-jwks', 'https://issuer.example/keys');
    fill('issuer-audiences', 'gateway');
    fill(
      'issuer-mappings',
      JSON.stringify([
        {
          name: 'build',
          match: {},
          project_id: 'project',
          limit_template: 'worker',
          route_groups: ['generation'],
          scopes: ['inference']
        }
      ])
    );
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(saveIssuer).toHaveBeenCalledTimes(1));
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Response unavailable')
    );
    expect(target.querySelector('fieldset')?.disabled).toBe(true);
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(saveIssuer).toHaveBeenCalledTimes(2));
    expect(vi.mocked(saveIssuer).mock.calls[1]).toEqual(
      vi.mocked(saveIssuer).mock.calls[0]
    );
  } finally {
    await unmount(view);
    client.clear();
    target.remove();
  }
});
