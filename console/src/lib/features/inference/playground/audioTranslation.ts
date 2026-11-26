import { gatewayFetch } from '#lib/management/gateway.js';
import {
  fail,
  publicGatewayHeaders,
  unwrapOutcome,
  validateApiKey,
  validatePublicRoute,
  type Outcome
} from './publicGateway';

export const translationFormats = [
  'json',
  'text',
  'srt',
  'vtt',
  'verbose_json'
] as const;
export type TranslationFormat = (typeof translationFormats)[number];

const maxAudioBytes = 25 * 1024 * 1024;

function validateTranslationInput(
  route: string,
  key: string,
  file: File,
  temperature: string
): Outcome<{ route: string; key: string }> {
  const validRoute = validatePublicRoute(route);
  if (!validRoute.ok) return validRoute;
  const validKey = validateApiKey(key);
  if (!validKey.ok) return validKey;
  if (file.size === 0 || file.size > maxAudioBytes)
    return fail('Choose a nonempty audio file no larger than 25 MiB.');
  if (temperature !== '') {
    const parsed = Number(temperature);
    if (!Number.isFinite(parsed) || parsed < 0 || parsed > 1)
      return fail('Temperature must be between 0 and 1.');
  }
  return { ok: true, value: { route: validRoute.value, key: validKey.value } };
}

export async function translateAudio(
  route: string,
  key: string,
  file: File,
  prompt: string,
  temperature: string,
  format: TranslationFormat,
  signal?: AbortSignal
): Promise<string> {
  const validated = unwrapOutcome(
    validateTranslationInput(route, key, file, temperature)
  );
  const body = new FormData();
  body.set('model', validated.route);
  body.set('file', file);
  if (prompt !== '') body.set('prompt', prompt);
  if (temperature !== '') body.set('temperature', temperature);
  body.set('response_format', format);
  const response = await gatewayFetch('/v1/audio/translations', {
    method: 'POST',
    headers: publicGatewayHeaders(validated.key),
    body,
    signal
  });
  if (!response.ok)
    throw new Error(
      `Audio translation failed (${response.status}). Check the route, key and audio format.`
    );
  return response.text();
}
