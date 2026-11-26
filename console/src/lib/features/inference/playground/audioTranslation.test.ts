import { afterEach, describe, expect, it, vi } from 'vitest';
import { gatewayFetch } from '#lib/management/gateway.js';
import { translateAudio } from './audioTranslation';

vi.mock('#lib/management/gateway.js', () => ({ gatewayFetch: vi.fn() }));

const sampleFile = new File(['audio'], 'audio.wav', { type: 'audio/wav' });

afterEach(() => vi.clearAllMocks());

describe('translateAudio', () => {
  it.each([
    {
      name: 'an invalid route slug',
      route: '../other',
      reason: 'active published route slug'
    },
    {
      name: 'a route longer than 100 characters',
      route: 'a'.repeat(101),
      reason: 'active published route slug'
    },
    {
      name: 'a key without the olp_ prefix',
      key: 'sk_test',
      reason: 'inference API key'
    },
    {
      name: 'a key longer than 512 characters',
      key: `olp_${'a'.repeat(509)}`,
      reason: 'inference API key'
    },
    {
      name: 'an empty audio file',
      file: new File([], 'empty.wav', { type: 'audio/wav' }),
      reason: 'nonempty audio file'
    },
    {
      name: 'a temperature below zero',
      temperature: '-0.1',
      reason: 'between 0 and 1'
    },
    {
      name: 'a temperature above one',
      temperature: '1.1',
      reason: 'between 0 and 1'
    },
    {
      name: 'a non-numeric temperature',
      temperature: 'warm',
      reason: 'between 0 and 1'
    }
  ])('rejects $name before dispatch', async (input) => {
    await expect(
      translateAudio(
        input.route ?? 'audio-route',
        input.key ?? 'olp_test',
        input.file ?? sampleFile,
        '',
        input.temperature ?? '',
        'json'
      )
    ).rejects.toThrow(input.reason);
    expect(gatewayFetch).not.toHaveBeenCalled();
  });

  it('sends multipart audio translation with gateway auth, no-store header, and max-length route', async () => {
    const validRoute = 'a'.repeat(100);
    const controller = new AbortController();
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response('{"text":"hello"}')
    );

    const text = await translateAudio(
      validRoute,
      'olp_test',
      sampleFile,
      'Transcribe cleanly',
      '0.2',
      'verbose_json',
      controller.signal
    );

    expect(text).toBe('{"text":"hello"}');
    const [path, options] = vi.mocked(gatewayFetch).mock.calls[0] ?? [];
    expect(path).toBe('/v1/audio/translations');
    expect(options?.method).toBe('POST');
    expect(options?.signal).toBe(controller.signal);
    const headers = new Headers(options?.headers);
    expect(headers.get('Authorization')).toBe('Bearer olp_test');
    expect(headers.get('Cache-Control')).toBe('no-store');
    expect(headers.has('Content-Type')).toBe(false);
    expect(options?.body).toBeInstanceOf(FormData);
    const form = options?.body as FormData;
    expect(form.get('model')).toBe(validRoute);
    expect(form.get('file')).toBe(sampleFile);
    expect(form.get('prompt')).toBe('Transcribe cleanly');
    expect(form.get('temperature')).toBe('0.2');
    expect(form.get('response_format')).toBe('verbose_json');
  });

  it('reports HTTP failure without exposing the upstream error body', async () => {
    vi.mocked(gatewayFetch).mockResolvedValueOnce(
      new Response('secret provider failure', { status: 415 })
    );

    await expect(
      translateAudio('audio-route', 'olp_test', sampleFile, '', '', 'text')
    ).rejects.toThrow('Audio translation failed (415).');
  });
});
