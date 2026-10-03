// LangChain embeddings: OpenAIEmbeddings and GoogleGenerativeAIEmbeddings. The
// Anthropic provider has no embeddings API.
import assert from 'node:assert/strict';
import { after, before, beforeEach, describe, test } from 'node:test';
import { GoogleGenerativeAIEmbeddings } from '@langchain/google-genai';
import { OpenAIEmbeddings } from '@langchain/openai';
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

const openai = (slug, extra) => new OpenAIEmbeddings({ model: slug, apiKey, maxRetries: 0, configuration: { baseURL: tap.baseURLs.openai }, ...extra });
const google = (slug) => new GoogleGenerativeAIEmbeddings({ model: slug, apiKey, baseUrl: tap.baseURLs.gemini, maxRetries: 0 });

describe('OpenAIEmbeddings', () => {
  test('embedQuery and embedDocuments return the vectors the upstream sent, in order', async () => {
    const one = await openai(models.openai).embedQuery(texts[0]);
    assertSameVector(one, reference[0], 'embedQuery');
    const many = await openai(models.openai).embedDocuments(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `document ${i}`));
    const { upstream } = await assertRelayed(tap, [
      { api: 'openai.embeddings', model: models.openai },
      { api: 'openai.embeddings', model: models.openai }
    ]);
    assert.deepEqual(upstream[1].body.input, texts);
  });

  test('the requested dimensions reach the upstream', async () => {
    const many = await openai(models.openai, { dimensions: 8 }).embedDocuments(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference8[i], `document ${i}`));
    const { upstream } = await assertRelayed(tap, [{ api: 'openai.embeddings', model: models.openai }]);
    assert.equal(upstream[0].body.dimensions, 8);
  });

  test('a Gemini route answers an OpenAI embeddings request by translation', async () => {
    const one = await openai(models.gemini).embedQuery(texts[0]);
    assertSameVector(one, reference[0], 'embedQuery');
    const many = await openai(models.gemini).embedDocuments(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `document ${i}`));
    await assertRelayed(tap, [
      { api: 'openai.embeddings', to: 'gemini.embed', model: models.gemini },
      { api: 'openai.embeddings', to: 'gemini.batch_embed', model: models.gemini }
    ]);
  });
});

describe('GoogleGenerativeAIEmbeddings', () => {
  test('embedQuery and embedDocuments reach the native endpoints of a strict embedding route', async () => {
    const one = await google(models.geminiEmbedStrict).embedQuery(texts[0]);
    assertSameVector(one, reference[0], 'embedQuery');
    const many = await google(models.geminiEmbedStrict).embedDocuments(texts);
    many.forEach((vector, i) => assertSameVector(vector, reference[i], `document ${i}`));
    await assertRelayed(tap, [
      { api: 'gemini.embed', model: models.geminiEmbedStrict },
      { api: 'gemini.batch_embed', model: models.geminiEmbedStrict }
    ]);
  });

  test('a transformed route refuses the native endpoints, as documented', async () => {
    const error = await google(models.gemini).embedQuery(texts[0]).then(
      () => assert.fail('the transformed route served a native Gemini embedding'),
      (rejection) => rejection
    );
    assert.equal(error.status, 400, error.message);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });
});
