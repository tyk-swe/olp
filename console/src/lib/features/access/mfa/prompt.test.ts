import { expect, it } from 'vitest';
import { confirmMFA, mfaPrompt } from './prompt.svelte';
const challenge = {
  challenge: 'opaque',
  expires_at: '2030-01-01T00:00:00Z',
  methods: ['totp' as const],
  enrollment_required: false,
  public_key: null
};
it('cancellation clears sensitive state and stale completion cannot finish a replacement', async () => {
  const controller = new AbortController();
  const first = confirmMFA({ challenge }, controller.signal);
  const rejected = expect(first).rejects.toThrow('cancelled');
  const old = mfaPrompt.current!;
  controller.abort();
  await rejected;
  expect(mfaPrompt.current).toBeNull();
  const next = confirmMFA({ challenge });
  const current = mfaPrompt.current!;
  old.finish();
  expect(mfaPrompt.current?.id).toBe(current.id);
  current.finish();
  await next;
  expect(mfaPrompt.current).toBeNull();
});
