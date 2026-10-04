// LlamaIndex.TS chat and the function-calling agent against the OpenAI,
// Anthropic and Google wire APIs. Every test asserts the client's own result,
// what the client sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { agent } from '@llamaindex/workflow';
import { extractText } from 'llamaindex';
import { afterTools, defaultReply, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed } from '../../lib/relay.mjs';
import { chatProviders, city, tap, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;
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

    test('chat completes a conversation', async () => {
      const response = await create().chat({ messages: messages('Say hello.') });
      assert.equal(extractText(response.message.content), defaultReply);
      await assertRelayed(tap, [request(false, { contains: ['You are terse.', 'Say hello.'] })]);
    });

    if (provider.streams !== false) {
      test('a streamed chat delivers the text in pieces', async () => {
        const pieces = [];
        for await (const chunk of await create().chat({ messages: messages('Say hello.'), stream: true })) pieces.push(chunk.delta);
        assert.ok(pieces.filter(Boolean).length > 1, 'the text arrives in more than one piece');
        assert.equal(pieces.join(''), defaultReply);
        await assertRelayed(tap, [request(true, { contains: ['Say hello.'] })]);
      });
    }

    test('a tool loop by hand completes and the tool schema arrives intact', async () => {
      const llm = create();
      const conversation = messages(weatherPrompt);
      const first = await llm.chat({ messages: conversation, tools: [weather] });
      const [call] = first.message.options.toolCall;
      assert.equal(call.name, 'get_weather');
      assert.deepEqual(typeof call.input === 'string' ? JSON.parse(call.input) : call.input, { city });
      const result = await weather.call({ city });
      const final = await llm.chat({
        messages: [...conversation, first.message, { role: 'user', content: result, options: { toolResult: { id: call.id, result, isError: false } } }],
        tools: [weather]
      });
      assert.equal(extractText(final.message.content), afterTools(`sunny in ${city}`));
      const { upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
      assert.deepEqual(
        upstream.map((u) => u.script),
        ['tool_call', 'tool_result']
      );
    });

    if (provider.agent !== false) {
      // Every step of the workflow agent streams.
      const agentRequest = () => request(true, { tools: ['get_weather'] });

      test('an agent runs the tool loop through streamed steps', async () => {
        const result = await agent({ llm: create(), tools: [weather], systemPrompt: 'You are terse.' }).run(weatherPrompt);
        assert.equal(result.data.result, afterTools(`sunny in ${city}`));
        const { upstream } = await assertRelayed(tap, [agentRequest(), agentRequest()]);
        assert.deepEqual(
          upstream.map((u) => u.script),
          ['tool_call', 'tool_result']
        );
      });

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
