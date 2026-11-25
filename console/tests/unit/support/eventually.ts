import { vi } from 'vitest';

export const eventually = <T>(check: () => T | Promise<T>) =>
  vi.waitFor(check, { interval: 1 });
