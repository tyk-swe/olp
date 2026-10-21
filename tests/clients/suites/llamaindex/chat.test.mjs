// LlamaIndex.TS chat and the function-calling agent against the OpenAI,
// Anthropic and Google wire APIs. Every test asserts the client's own result,
// what the client sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { agent } from '@llamaindex/workflow';
import { afterTools, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed } from '../../lib/relay.mjs';
import { chatProviders, tap, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});
const messages = (user) => [
  { role: 'system', content: 'You are terse.' },
  { role: 'user', content: user }
];

for (const provider of chatProviders) {
  describe(provider.name, () => {
    const request = (stream, extra = {}) => ({
      api: provider.api(stream),
      model: provider.slug,
      stream,
      maxTokens: 77,
      temperature: provider.sendsTemperature === false ? undefined : 0.2,
      contains: ['You are terse.'],
      ...extra
    });
    const create = () => provider.create({ temperature: 0.2, maxTokens: 77 });

    if (provider.agent !== false) {
      // Every step of the workflow agent streams.
      const agentRequest = () => request(true, { tools: ['get_weather'] });

      test('an agent runs parallel tool calls in one round', async () => {
        const prompt = `Weather in two cities. ${script.tool('get_weather', { city: 'Oslo' })} ${script.also('get_weather', { city: 'Lima' })}`;
        const result = await agent({ llm: create(), tools: [weather], systemPrompt: 'You are terse.' }).run(prompt);
        assert.equal(result.data.result, afterTools('sunny in Oslo', 'sunny in Lima'));
        await assertRelayed(tap, [agentRequest(), agentRequest()]);
      });
    }

    for (const [name, key, status] of [
      ['an unknown key', 'olp_unknown', 401],
      ['a key without access to the route', restrictedApiKey, 403]
    ]) {
      test(`${name} is refused before the upstream, as a typed error`, async () => {
        const error = await provider.create({ key }).chat({ messages: messages('Say hello.') }).then(
          () => assert.fail('the gateway accepted the request'),
          (rejection) => rejection
        );
        assert.equal(error.status ?? error.statusCode ?? error.code, status, `${error.name}: ${error.message}`);
        const [exchange] = await tap.sent();
        assert.equal(exchange.status, status);
        assert.ok(error.message.includes(JSON.parse(exchange.responseText).error.message), 'the error carries the gateway message');
        assert.deepEqual(await recorded(), [], 'the upstream was never reached');
      });
    }
  });
}
