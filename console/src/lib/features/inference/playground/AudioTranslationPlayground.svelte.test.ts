import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, test, vi } from 'vitest';
import AudioTranslationPlayground from './AudioTranslationPlayground.svelte';

afterEach(() => vi.unstubAllGlobals());

test('uploads from the billable audio form and displays the native text result', async () => {
  const host = document.createElement('div');
  document.body.append(host);
  const request = vi.fn().mockResolvedValue(new Response('WEBVTT\n\nhello\n'));
  vi.stubGlobal('fetch', request);
  const component = mount(AudioTranslationPlayground, {
    target: host,
    props: { route: 'translation-route' }
  });
  try {
    flushSync();
    expect(host.textContent).toContain('billed on execution');
    const key = host.querySelector<HTMLInputElement>('#translation-key')!;
    key.value = 'olp_test';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    const file = new File(['original-audio'], 'source.wav', {
      type: 'audio/wav'
    });
    const upload = host.querySelector<HTMLInputElement>('#translation-file')!;
    Object.defineProperty(upload, 'files', {
      value: [file],
      configurable: true,
      writable: true
    });
    upload.dispatchEvent(new Event('change', { bubbles: true }));
    const format = host.querySelector<HTMLSelectElement>(
      '#translation-format'
    )!;
    format.value = 'vtt';
    format.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    expect(
      host.querySelector<HTMLButtonElement>('button[type="submit"]')!.disabled
    ).toBe(false);
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(host.querySelector('pre')?.textContent).toBe('WEBVTT\n\nhello\n')
    );
    expect(request).toHaveBeenCalledOnce();
    expect(request.mock.calls[0][1].body.get('model')).toBe(
      'translation-route'
    );
    expect(request.mock.calls[0][1].body.get('response_format')).toBe('vtt');
  } finally {
    await unmount(component);
    host.remove();
  }
});
