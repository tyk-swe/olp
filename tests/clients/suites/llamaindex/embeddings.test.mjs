// LlamaIndex.TS embeddings: OpenAIEmbedding and GeminiEmbedding. The Anthropic
// provider has no embeddings API.
import assert from 'node:assert/strict';
import { after, before, beforeEach, describe, test } from 'node:test';
import { GeminiEmbedding } from '@llamaindex/google';
import { OpenAIEmbedding } from '@llamaindex/openai';
import { apiKey, models, recorded, resetRecorded } from '../../lib/harness.mjs';
import { assertRelayed, assertSameVector, referenceEmbeddings } from '../../lib/relay.mjs';
import { tap } from './providers.mjs';

const texts = ['alpha', 'beta', 'Zürich 東京 🚀'];
let reference;
let reference8;
before(async () => {
  reference = await referenceEmbeddings(texts);
  reference8 = await referenceEmbeddings(texts, 8);
});
after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const openai = (slug, extra) => new OpenAIEmbedding({ model: slug, apiKey, baseURL: tap.baseURLs.openai, maxRetries: 0, ...extra });
const gemini = (slug) => new GeminiEmbedding({ model: slug, apiKey, httpOptions: { baseUrl: tap.baseURLs.gemini } });

describe('OpenAIEmbedding', () => {
  test('getTextEmbedding and getTextEmbeddings return the vectors the upstream sent, in order', async () => {
    const one = await openai(models.openai).getTextEmbedding(texts[0]);
    assertSameVector(one, reference[0], 'getTextEmbedding');
    const many = await openai(models.openai).getTextEmbeddings(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `text ${i}`));
    const { upstream } = await assertRelayed(tap, [
      { api: 'openai.embeddings', model: models.openai },
      { api: 'openai.embeddings', model: models.openai }
    ]);
    assert.deepEqual(upstream[1].body.input, texts);
  });

  test('the requested dimensions reach the upstream', async () => {
    const many = await openai(models.openai, { dimensions: 8 }).getTextEmbeddings(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference8[i], `text ${i}`));
    const { upstream } = await assertRelayed(tap, [{ api: 'openai.embeddings', model: models.openai }]);
    assert.equal(upstream[0].body.dimensions, 8);
  });

  test('a Gemini route answers an OpenAI embeddings request by translation', async () => {
    const one = await openai(models.gemini).getTextEmbedding(texts[0]);
    assertSameVector(one, reference[0], 'getTextEmbedding');
    const many = await openai(models.gemini).getTextEmbeddings(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `text ${i}`));
    await assertRelayed(tap, [
      { api: 'openai.embeddings', to: 'gemini.embed', model: models.gemini },
      { api: 'openai.embeddings', to: 'gemini.batch_embed', model: models.gemini }
    ]);
  });
});

describe('GeminiEmbedding', () => {
  test('the native batch endpoint of a strict embedding route returns the vectors in order', async () => {
    // LlamaIndex sends one text as a batch of one, so only batchEmbedContents is used.
    const one = await gemini(models.geminiEmbedStrict).getTextEmbedding(texts[0]);
    assertSameVector(one, reference[0], 'getTextEmbedding');
    const many = await gemini(models.geminiEmbedStrict).getTextEmbeddings(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `text ${i}`));
    await assertRelayed(tap, [
      { api: 'gemini.batch_embed', model: models.geminiEmbedStrict },
      { api: 'gemini.batch_embed', model: models.geminiEmbedStrict }
    ]);
  });

  test('a transformed route refuses the native endpoints, as documented', async () => {
    const error = await gemini(models.gemini).getTextEmbedding(texts[0]).then(
      () => assert.fail('the transformed route served a native Gemini embedding'),
      (rejection) => rejection
    );
    assert.equal(error.status, 400, error.message);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });
});
