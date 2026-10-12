// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, vi } from 'vitest';
import IncreaseProbe from './test/IncreaseProbe.svelte';
import { createIncrease } from './api';
vi.mock('./api', () => ({
  listIncreases: vi.fn(async () => ({ items: [], next_cursor: null })),
  createIncrease: vi.fn(),
  revokeIncrease: vi.fn()
}));
vi.mock('../session/useRole.svelte', () => ({
  useRole: () => ({ allows: () => true })
}));
it('retries identical grant evidence after a lost response', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const target = document.createElement('div');
  document.body.append(target);
  const component = mount(IncreaseProbe, { target, props: { client } });
  try {
    flushSync();
    for (const [id, value] of [
      ['increase-resource', '00000000-0000-4000-8000-000000000001'],
      ['increase-amount', '0.000000000001'],
      ['increase-reason', 'Incident capacity']
    ]) {
      const input = target.querySelector<HTMLInputElement>(`#${id}`)!;
      input.value = value;
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }
    vi.mocked(createIncrease)
      .mockRejectedValueOnce(new Error('Lost response'))
      .mockRejectedValueOnce(new Error('Still unavailable'));
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Retry increase')
    );
    flushSync();
    expect(target.querySelector('fieldset')!.disabled).toBe(true);
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(createIncrease).toHaveBeenCalledTimes(2));
    const first = vi.mocked(createIncrease).mock.calls[0];
    expect(vi.mocked(createIncrease).mock.calls[1]).toEqual(first);
    expect(first[0].amount).toBe('0.000000000001');
    await new Promise((resolve) => setTimeout(resolve, 0));
    vi.mocked(createIncrease).mockResolvedValueOnce({
      id: 'saved-increase',
      owner_id: 'owner',
      project_id: null,
      organization_id: null,
      target: first[0].target,
      window_kind: 'day',
      window_id: Date.parse('2026-10-11T00:00:00Z') / 1000,
      amount: first[0].amount,
      reason: first[0].reason,
      starts_at: '2026-10-11T00:00:00Z',
      expires_at: '2026-10-12T00:00:00Z',
      window_ends_at: '2026-10-12T00:00:00Z',
      created_at: '2026-10-11T00:00:00Z',
      created_by: 'owner',
      revoked_at: null,
      revoked_by: null,
      etag: 'saved-etag'
    });
    vi.spyOn(client, 'invalidateQueries').mockRejectedValueOnce(
      new Error('Refresh unavailable')
    );
    target
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(target.textContent).toContain('refreshing the view failed')
    );
    expect(target.textContent).toContain('Increase recorded until');
    expect(target.querySelector('fieldset')!.disabled).toBe(false);
  } finally {
    await unmount(component);
    client.clear();
    target.remove();
  }
});
