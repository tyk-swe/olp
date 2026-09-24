import { afterEach, expect, test, vi } from 'vitest';
import { translateAudio } from './audioTranslation';

afterEach(() => vi.unstubAllGlobals());

test('uploads the original file and native fields, leaving multipart headers to the browser', async () => {
  const file = new File([new Uint8Array([0, 1, 255, 3])], 'speech.wav', {
    type: 'audio/wav'
  });
  const request = vi.fn().mockResolvedValue(new Response('WEBVTT\nhello\n'));
  vi.stubGlobal('fetch', request);
  expect(
    await translateAudio(
      'audio-route',
      'olp_test',
      file,
      ' English hint ',
      '0.000',
      'vtt'
    )
  ).toBe('WEBVTT\nhello\n');
  expect(request).toHaveBeenCalledOnce();
  const [url, options] = request.mock.calls[0];
  expect(url).toBe('/v1/audio/translations');
  expect(options.headers).toEqual({ Authorization: 'Bearer olp_test' });
  expect(options.body.get('file')).toBe(file);
  expect([...options.body.keys()]).toEqual([
    'model',
    'file',
    'prompt',
    'temperature',
    'response_format'
  ]);
  expect(options.body.get('temperature')).toBe('0.000');
  expect(options.body.get('prompt')).toBe(' English hint ');
  expect(options.credentials).toBe('omit');
});

test('omits empty optional controls and rejects invalid temperature before dispatch', async () => {
  const file = new File(['audio'], 'speech.wav');
  const request = vi.fn().mockResolvedValue(new Response('{"text":"hello"}'));
  vi.stubGlobal('fetch', request);
  await translateAudio('audio-route', 'olp_test', file, '', '', 'json');
  const body = request.mock.calls[0][1].body;
  expect(body.has('prompt')).toBe(false);
  expect(body.has('temperature')).toBe(false);
  await expect(
    translateAudio('audio-route', 'olp_test', file, '', 'NaN', 'json')
  ).rejects.toThrow('Temperature');
  expect(request).toHaveBeenCalledOnce();
});
