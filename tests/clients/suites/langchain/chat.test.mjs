// LangChain chat models: plain chat, streaming, tool calling by hand and through
// `createAgent`, and structured output, each against the OpenAI, Anthropic and
// Google wire APIs. Every test asserts the client's own result, what the client
// sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { HumanMessage, SystemMessage } from '@langchain/core/messages';
import { afterTools, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed, textOf } from '../../lib/relay.mjs';
import { chatProviders, tap, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});
const terse = new SystemMessage('You are terse.');

for (const provider of chatProviders) {
  describe(provider.name, () => {
    const request = (stream, extra = {}) => ({ api: provider.api(stream), model: provider.slug, stream, maxTokens: 77, temperature: 0.2, contains: ['You are terse.'], ...extra });
    const create = () => provider.create({ temperature: 0.2, maxTokens: 77 });

    test('parallel tool calls in one round all complete', async () => {
      const prompt = `Weather in two cities. ${script.tool('get_weather', { city: 'Oslo' })} ${script.also('get_weather', { city: 'Lima' })}`;
      const model = create().bindTools([weather]);
      const call = await model.invoke([terse, new HumanMessage(prompt)]);
      assert.deepEqual(
        call.tool_calls.map((c) => c.args.city),
        ['Oslo', 'Lima']
      );
      const results = await Promise.all(call.tool_calls.map((c) => weather.invoke(c)));
      const final = await model.invoke([terse, new HumanMessage(prompt), call, ...results]);
      assert.equal(textOf(final.content), afterTools('sunny in Oslo', 'sunny in Lima'));
      await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
    });

    for (const [name, key, status] of [
      ['an unknown key', 'olp_unknown', 401],
      ['a key without access to the route', restrictedApiKey, 403]
    ]) {
      test(`${name} is refused before the upstream, as a typed error`, async () => {
        const error = await provider.create({ key }).invoke('Say hello.').then(
          () => assert.fail('the gateway accepted the request'),
          (rejection) => rejection
        );
        assert.equal(error.status ?? error.statusCode, status, `${error.name}: ${error.message}`);
        const [exchange] = await tap.sent();
        assert.equal(exchange.status, status);
        assert.ok(error.message.includes(JSON.parse(exchange.responseText).error.message), 'the error carries the gateway message');
        assert.deepEqual(await recorded(), [], 'the upstream was never reached');
      });
    }
  });
}
