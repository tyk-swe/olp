// Vercel AI SDK embeddings: `embed` and `embedMany` through the OpenAI and
// Google providers. The Anthropic provider has no embeddings API.
import assert from 'node:assert/strict';
import { after, before, beforeEach, describe, test } from 'node:test';
import { APICallError, embed, embedMany } from 'ai';
import { models, recorded, resetRecorded } from '../../lib/harness.mjs';
import { assertRelayed, assertSameVector, referenceEmbeddings } from '../../lib/relay.mjs';
import { google, openai, tap } from './providers.mjs';

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

describe('OpenAI provider', () => {
  const model = (slug) => openai.embedding(slug);

  test('embed returns the vector and the usage the gateway reports', async () => {
    const result = await embed({ model: model(models.openai), value: texts[0], maxRetries: 0 });
    assertSameVector(result.embedding, reference[0], 'embed');
    assert.ok(result.usage.tokens > 0, 'usage is reported');
    const { upstream } = await assertRelayed(tap, [{ api: 'openai.embeddings', model: models.openai }]);
    assert.deepEqual(upstream[0].body.input, [texts[0]]);
  });

  test('embedMany keeps the order of the inputs and the requested dimensions', async () => {
    const result = await embedMany({ model: model(models.openai), values: texts, maxRetries: 0, providerOptions: { openai: { dimensions: 8 } } });
    assert.equal(result.embeddings.length, texts.length);
    result.embeddings.forEach((vector, i) => assertSameVector(vector, reference8[i], `embedding ${i}`));
    const { upstream } = await assertRelayed(tap, [{ api: 'openai.embeddings', model: models.openai }]);
    assert.deepEqual(upstream[0].body.input, texts);
    assert.equal(upstream[0].body.dimensions, 8);
  });

  test('a Gemini route answers an OpenAI embeddings request by translation, with the usage the Gemini upstream reports', async () => {
    // The scripted vendors count the tokens of a text alike, so the OpenAI
    // upstream says what the Gemini upstream should be heard to say.
    const expected = {
      one: (await embed({ model: model(models.openai), value: texts[0], maxRetries: 0 })).usage.tokens,
      many: (await embedMany({ model: model(models.openai), values: texts, maxRetries: 0 })).usage.tokens
    };
    await resetRecorded();
    tap.reset();

    const one = await embed({ model: model(models.gemini), value: texts[0], maxRetries: 0 });
    assertSameVector(one.embedding, reference[0], 'embed');
    assert.ok(one.usage.tokens > 0, 'usage is reported');
    assert.equal(one.usage.tokens, expected.one);
    const many = await embedMany({ model: model(models.gemini), values: texts, maxRetries: 0 });
    many.embeddings.forEach((vector, i) => assertSameVector(vector, reference[i], `embedding ${i}`));
    assert.equal(many.usage.tokens, expected.many);
    const { upstream } = await assertRelayed(tap, [
      { api: 'openai.embeddings', to: 'gemini.embed', model: models.gemini },
      { api: 'openai.embeddings', to: 'gemini.batch_embed', model: models.gemini }
    ]);
    assert.equal(upstream[0].body.content.parts[0].text, texts[0]);
    assert.deepEqual(
      upstream[1].body.requests.map((request) => request.content.parts[0].text),
      texts
    );
  });
});

describe('Google provider', () => {
  test('embed and embedMany reach the native endpoints of a strict embedding route', async () => {
    const model = google.embedding(models.geminiEmbedStrict);
    const one = await embed({ model, value: texts[0], maxRetries: 0 });
    assertSameVector(one.embedding, reference[0], 'embed');
    const many = await embedMany({ model, values: texts, maxRetries: 0 });
    assert.equal(many.embeddings.length, texts.length);
    many.embeddings.forEach((vector, i) => assertSameVector(vector, reference[i], `embedding ${i}`));
    await assertRelayed(tap, [
      { api: 'gemini.embed', model: models.geminiEmbedStrict },
      { api: 'gemini.batch_embed', model: models.geminiEmbedStrict }
    ]);
  });

  test('a transformed route refuses the native endpoints, as documented', async () => {
    const error = await embed({ model: google.embedding(models.gemini), value: texts[0], maxRetries: 0 }).then(
      () => assert.fail('the transformed route served a native Gemini embedding'),
      (rejection) => rejection
    );
    assert.ok(APICallError.isInstance(error), `${error}`);
    assert.equal(error.statusCode, 400);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });
});
