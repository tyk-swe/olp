import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import OpenAI, { toFile } from 'openai';

const client = new OpenAI({
  baseURL: `${process.env.OLP_TRANSLATION_BASE}/v1`,
  apiKey: process.env.OLP_TRANSLATION_KEY,
  maxRetries: 0,
  timeout: 15000
});
const format = process.env.OLP_TRANSLATION_FORMAT;
const audio = await readFile(new URL('../fixtures/media/tiny-pcm.wav', import.meta.url));
const result = await client.audio.translations.create({
  file: await toFile(audio, 'original.wav', { type: 'audio/wav' }),
  model: process.env.OLP_TRANSLATION_ROUTE,
  temperature: 0,
  response_format: format
});
if (format === 'json' || format === 'verbose_json') {
  assert.deepEqual(result, JSON.parse(process.env.OLP_TRANSLATION_EXPECT));
} else {
  assert.equal(result, process.env.OLP_TRANSLATION_EXPECT);
}
console.log(`openai-js-7.4.0: strict audio translation ${format} passed`);
