import { afterEach } from 'vitest';
import { toasts } from '#lib/components/toasts.svelte.js';
import { authLifecycle } from '#lib/session/lifecycle.js';

afterEach(() => {
  toasts.clear();
  authLifecycle.reset();
});
