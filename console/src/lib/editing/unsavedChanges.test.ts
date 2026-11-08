import { afterEach, expect, it, vi } from 'vitest';
import type { beforeNavigate } from '$app/navigation';

type BeforeNavigate = Parameters<Parameters<typeof beforeNavigate>[0]>[0];

const guards = vi.hoisted(() => [] as ((navigation: BeforeNavigate) => void)[]);
vi.mock('$app/navigation', () => ({
  beforeNavigate: (guard: (navigation: BeforeNavigate) => void) =>
    guards.push(guard)
}));

const { guardUnsavedChanges } = await import('./unsavedChanges.js');

afterEach(() => {
  guards.length = 0;
  vi.unstubAllGlobals();
});

function navigate(from: string, to: string) {
  const cancel = vi.fn();
  const navigation = {
    type: 'link',
    shallow: false,
    from: { url: new URL(from, 'http://console.test') },
    to: { url: new URL(to, 'http://console.test') },
    cancel
  } as unknown as BeforeNavigate;
  guards.forEach((guard) => guard(navigation));
  return cancel;
}

it('keeps a dirty draft when switching views of the same page', () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  guardUnsavedChanges(() => true);

  const cancel = navigate('/providers/p1', '/providers/p1?view=models');

  expect(confirm).not.toHaveBeenCalled();
  expect(cancel).not.toHaveBeenCalled();
});

it('asks before leaving a dirty draft for another page', () => {
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  guardUnsavedChanges(() => true);

  const cancel = navigate('/providers/p1', '/providers');

  expect(confirm).toHaveBeenCalledOnce();
  expect(cancel).toHaveBeenCalledOnce();
});
