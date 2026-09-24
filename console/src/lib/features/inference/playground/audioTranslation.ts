export const translationFormats = [
  'json',
  'text',
  'srt',
  'vtt',
  'verbose_json'
] as const;
export type TranslationFormat = (typeof translationFormats)[number];

export async function translateAudio(
  route: string,
  key: string,
  file: File,
  prompt: string,
  temperature: string,
  format: TranslationFormat,
  signal?: AbortSignal
): Promise<string> {
  if (!/^[a-z0-9][a-z0-9._-]{0,127}$/.test(route))
    throw new Error('Choose an active published route slug.');
  if (!key.startsWith('olp_') || key.length > 512)
    throw new Error('Enter an inference API key for this route.');
  if (file.size === 0 || file.size > 25 * 1024 * 1024)
    throw new Error('Choose a nonempty audio file no larger than 25 MiB.');
  if (
    temperature !== '' &&
    (!Number.isFinite(Number(temperature)) ||
      Number(temperature) < 0 ||
      Number(temperature) > 1)
  )
    throw new Error('Temperature must be between 0 and 1.');
  const body = new FormData();
  body.set('model', route);
  body.set('file', file);
  if (prompt !== '') body.set('prompt', prompt);
  if (temperature !== '') body.set('temperature', temperature);
  body.set('response_format', format);
  const response = await fetch('/v1/audio/translations', {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}` },
    body,
    signal,
    credentials: 'omit',
    redirect: 'error',
    cache: 'no-store'
  });
  if (!response.ok)
    throw new Error(
      `Audio translation failed (${response.status}). Check the route, key and audio format.`
    );
  return response.text();
}
