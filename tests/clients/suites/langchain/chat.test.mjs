// LangChain chat models: plain chat, streaming, tool calling by hand and through
// `createAgent`, and structured output, each against the OpenAI, Anthropic and
// Google wire APIs. Every test asserts the client's own result, what the client
// sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { HumanMessage, SystemMessage, ToolMessage } from '@langchain/core/messages';
import { createAgent } from 'langchain';
import { afterTools, defaultReply, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed, images, png, textOf } from '../../lib/relay.mjs';
import { chatProviders, city, tap, ticket, ticketSchema, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;
const terse = new SystemMessage('You are terse.');

for (const provider of chatProviders) {
  describe(provider.name, () => {
    const request = (stream, extra = {}) => ({ api: provider.api(stream), model: provider.slug, stream, maxTokens: 77, temperature: 0.2, contains: ['You are terse.'], ...extra });
    const create = () => provider.create({ temperature: 0.2, maxTokens: 77 });

    test('invoke completes a chat', async () => {
      const reply = await create().invoke([terse, new HumanMessage('Say hello.')]);
      assert.equal(textOf(reply.content), defaultReply);
      assert.ok(reply.usage_metadata.total_tokens > 0, 'usage is reported');
      await assertRelayed(tap, [request(false, { contains: ['You are terse.', 'Say hello.'] })]);
    });

    test('stream delivers the text in pieces', async () => {
      const pieces = [];
      for await (const chunk of await create().stream([terse, new HumanMessage('Say hello.')])) pieces.push(textOf(chunk.content));
      assert.ok(pieces.filter(Boolean).length > 1, 'the text arrives in more than one piece');
      assert.equal(pieces.join(''), defaultReply);
      await assertRelayed(tap, [request(true, { contains: ['Say hello.'] })]);
    });

    test('an image arrives intact', async () => {
      const content = [{ type: 'text', text: 'Describe the image.' }, { type: 'image_url', image_url: { url: `data:image/png;base64,${png.toString('base64')}` } }];
      const reply = await create().invoke([terse, new HumanMessage({ content })]);
      assert.equal(textOf(reply.content), defaultReply);
      const { upstream } = await assertRelayed(tap, [request(false, { contains: ['Describe the image.'] })]);
      assert.deepEqual(images(provider.api(false), upstream[0].body), [{ mediaType: 'image/png', data: png.toString('base64') }]);
    });

    test('a tool loop by hand completes and the tool schema arrives intact', async () => {
      const model = create().bindTools([weather]);
      const messages = [terse, new HumanMessage(weatherPrompt)];
      const call = await model.invoke(messages);
      assert.equal(call.tool_calls.length, 1);
      assert.equal(call.tool_calls[0].name, 'get_weather');
      assert.deepEqual(call.tool_calls[0].args, { city });
      const result = await weather.invoke(call.tool_calls[0]);
      const final = await model.invoke([...messages, call, result]);
      assert.equal(textOf(final.content), afterTools(`sunny in ${city}`));
      const { upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
      assert.deepEqual(
        upstream.map((u) => u.script),
        ['tool_call', 'tool_result']
      );
    });

    test('a streamed tool call is assembled from its fragments', async () => {
      let call;
      for await (const chunk of await create().bindTools([weather]).stream([terse, new HumanMessage(weatherPrompt)])) call = call ? call.concat(chunk) : chunk;
      assert.equal(call.tool_calls.length, 1);
      assert.deepEqual(call.tool_calls[0].args, { city });
      await assertRelayed(tap, [request(true, { tools: ['get_weather'] })]);
    });

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

    test('createAgent runs the tool loop', async () => {
      const agent = createAgent({ model: create(), tools: [weather], systemPrompt: 'You are terse.' });
      const { messages } = await agent.invoke({ messages: [{ role: 'user', content: weatherPrompt }] });
      assert.equal(textOf(messages.at(-1).content), afterTools(`sunny in ${city}`));
      assert.ok(messages.some((message) => message.type === 'tool' && textOf(message.content) === `sunny in ${city}`), 'the tool result is in the history');
      await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
    });

    test('structured output returns the object the schema describes', async () => {
      const result = await create().withStructuredOutput(ticketSchema).invoke([terse, new HumanMessage('File a ticket.')]);
      assert.deepEqual(result, ticket);
      const { upstream } = await assertRelayed(tap, [request(false, { contains: ['File a ticket.'] })]);
      assert.equal(upstream[0].script, provider.structuredScript ?? 'structured', 'the upstream was asked for the schema');
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
