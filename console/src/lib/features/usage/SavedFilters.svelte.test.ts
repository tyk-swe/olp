// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import SavedFilters from './SavedFilters.svelte';
import { saveFilters } from './savedFilters';

const session = '11111111-1111-1111-1111-111111111111';
vi.mock('$lib/features/access/session/lifecycle', () => ({
  authLifecycle: {
    subscribe(
      callback: (snapshot: { phase: string; sessionId: string }) => void
    ) {
      callback({
        phase: 'authenticated',
        sessionId: '11111111-1111-1111-1111-111111111111'
      });
      return () => {};
    }
  }
}));

describe('saved filter controls', () => {
  let host: HTMLElement;
  let component: ReturnType<typeof mount> | undefined;
  beforeEach(() => {
    window.sessionStorage.clear();
    host = document.createElement('div');
    document.body.append(host);
  });
  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    host.remove();
  });

  it('restores and applies a saved view through its labelled select', async () => {
    saveFilters(window.sessionStorage, session, 'usage', [
      { name: 'Weekly', search: '?route=assistant&cursor=discard' }
    ]);
    const apply = vi.fn();
    flushSync(() => {
      component = mount(SavedFilters, {
        target: host,
        props: { scope: 'usage', search: '', apply }
      });
    });
    const select = host.querySelector<HTMLSelectElement>(
      'select[aria-label="Saved view"]'
    )!;
    expect([...select.options].map((option) => option.text)).toContain(
      'Weekly'
    );
    flushSync(() => {
      select.value = 'Weekly';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    });
    const button = [...host.querySelectorAll('button')].find(
      (candidate) => candidate.textContent?.trim() === 'Apply view'
    )!;
    expect(button.disabled).toBe(false);
    flushSync(() => button.click());
    expect(apply).toHaveBeenCalledWith('?route=assistant');
  });
});
