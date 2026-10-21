// LangChain embeddings: OpenAIEmbeddings and GoogleGenerativeAIEmbeddings. The
// Anthropic provider has no embeddings API.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { GoogleGenerativeAIEmbeddings } from '@langchain/google-genai';
import { apiKey, models, recorded, resetRecorded } from '../../lib/harness.mjs';
import { tap } from './providers.mjs';

const texts = ['alpha', 'beta', 'Zürich 東京 🚀'];
after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});
const google = (slug) => new GoogleGenerativeAIEmbeddings({ model: slug, apiKey, baseUrl: tap.baseURLs.gemini, maxRetries: 0 });

describe('GoogleGenerativeAIEmbeddings', () => {

  test('a transformed route refuses the native endpoints, as documented', async () => {
    const error = await google(models.gemini).embedQuery(texts[0]).then(
      () => assert.fail('the transformed route served a native Gemini embedding'),
      (rejection) => rejection
    );
    assert.equal(error.status, 400, error.message);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });
});
