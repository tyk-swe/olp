// Vercel AI SDK chat, tool calling and structured output through each of its
// OpenAI, Anthropic and Google providers: the client's own answer, what the
// client sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { APICallError, generateText, stepCountIs, streamText } from 'ai';
import { afterTools, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed } from '../../lib/relay.mjs';
import { chatProviders, tap, vendors, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const common = { maxRetries: 0, maxOutputTokens: 77, temperature: 0.2, instructions: 'You are terse.' };

for (const provider of chatProviders) {
  describe(provider.name, () => {
    const request = (stream, extra = {}) => ({ api: provider.api(stream), model: provider.slug, stream, maxTokens: 77, temperature: 0.2, contains: ['You are terse.'], ...extra });

    test('concurrent calls each get their own answer', async () => {
      // Sixteen calls in flight at once, half of them streamed, each scripted to answer with its own number.
      const answers = await Promise.all(
        Array.from({ length: 16 }, async (_, i) => {
          const prompt = `Call ${i}. ${script.reply(`answer-${i}`)}`;
          if (i % 2 === 0) return (await generateText({ model: provider.model(), prompt, maxRetries: 0 })).text;
          let text = '';
          for await (const piece of streamText({ model: provider.model(), prompt, maxRetries: 0 }).textStream) text += piece;
          return text;
        })
      );
      assert.deepEqual(
        answers,
        Array.from({ length: 16 }, (_, i) => `answer-${i}`)
      );
      const upstream = await recorded();
      assert.equal(upstream.length, 16);
      assert.ok(upstream.every((u) => u.authorized && !u.leaked_client_credential && u.status === 200), 'every upstream request was clean');
      assert.equal(new Set(upstream.map((u) => JSON.stringify(u.body).match(/Call (\d+)\./)[1])).size, 16, 'each call reached the upstream once');
    });

    test('parallel tool calls in one round all complete', async () => {
      const prompt = `Weather in two cities. ${script.tool('get_weather', { city: 'Oslo' })} ${script.also('get_weather', { city: 'Lima' })}`;
      const result = await generateText({ model: provider.model(), tools: { get_weather: weather }, stopWhen: stepCountIs(4), prompt, ...common });
      assert.deepEqual(
        result.steps[0].toolCalls.map((call) => call.input.city),
        ['Oslo', 'Lima']
      );
      assert.equal(result.text, afterTools('sunny in Oslo', 'sunny in Lima'));
      await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
    });

    for (const [name, key, status] of [
      ['an unknown key', 'olp_unknown', 401],
      ['a key without access to the route', restrictedApiKey, 403]
    ]) {
      test(`${name} is refused before the upstream, as a typed error`, async () => {
        const error = await generateText({ model: provider.model(vendors(key)), prompt: 'Say hello.', maxRetries: 0 }).then(
          () => assert.fail('the gateway accepted the request'),
          (rejection) => rejection
        );
        assert.ok(APICallError.isInstance(error), `${error}`);
        assert.equal(error.statusCode, status);
        const [exchange] = await tap.sent();
        assert.equal(exchange.status, status);
        assert.equal(error.message, JSON.parse(exchange.responseText).error.message, 'the SDK parsed the gateway error envelope');
        assert.deepEqual(await recorded(), [], 'the upstream was never reached');
      });
    }
  });
}
