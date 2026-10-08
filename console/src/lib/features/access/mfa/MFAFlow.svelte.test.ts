import { mount, unmount, flushSync } from 'svelte';
import { expect, it, vi } from 'vitest';
import MFAFlow from './MFAFlow.svelte';
import { verifyMFA } from './api';
vi.mock('./api', () => ({ verifyMFA: vi.fn(), enrollMFA: vi.fn() }));
it('keeps one-time recovery codes until explicit acknowledgment', async () => {
  vi.mocked(verifyMFA).mockResolvedValue({
    enrolled: true,
    recovery_codes: ['11111111-22222222-33333333-44444444']
  });
  const host = document.createElement('div');
  document.body.append(host);
  const complete = vi.fn();
  const component = mount(MFAFlow, {
    target: host,
    props: {
      enrollment: {
        kind: 'totp',
        challenge: 'opaque-proof',
        expires_at: '2030-01-01T00:00:00Z',
        secret: 'TEST-ONLY-SEED',
        qr_code: 'data:image/png;base64,AA=='
      },
      onComplete: complete
    }
  });
  flushSync();
  const input = host.querySelector<HTMLInputElement>('#mfa-code')!;
  input.value = '123456';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.querySelector('textarea')?.value).toContain('11111111')
  );
  expect(complete).not.toHaveBeenCalled();
  expect(verifyMFA).toHaveBeenCalledWith(
    { challenge: 'opaque-proof', method: 'totp', code: '123456' },
    expect.any(AbortSignal)
  );
  const acknowledge = [...host.querySelectorAll('button')].find((b) =>
    b.textContent?.includes('I saved my recovery codes')
  )!;
  acknowledge.click();
  expect(complete).toHaveBeenCalledOnce();
  await unmount(component);
  host.remove();
});
