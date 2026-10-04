// Vercel AI SDK chat, tool calling and structured output through each of its
// OpenAI, Anthropic and Google providers: the client's own answer, what the
// client sent, and what the scripted upstream received.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { APICallError, Output, generateText, jsonSchema, stepCountIs, streamText } from 'ai';
import { afterTools, defaultReply, recorded, resetRecorded, restrictedApiKey, script } from '../../lib/harness.mjs';
import { assertRelayed, images, png } from '../../lib/relay.mjs';
import { chatProviders, city, tap, ticket, ticketSchema, vendors, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const common = { maxRetries: 0, maxOutputTokens: 77, temperature: 0.2, instructions: 'You are terse.' };
const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;

for (const provider of chatProviders) {
  describe(provider.name, () => {
    const request = (stream, extra = {}) => ({ api: provider.api(stream), model: provider.slug, stream, maxTokens: 77, temperature: 0.2, contains: ['You are terse.'], ...extra });

    test('generateText completes a chat', async () => {
      const result = await generateText({ model: provider.model(), prompt: 'Say hello.', ...common });
      assert.equal(result.text, defaultReply);
      assert.equal(result.finishReason, 'stop');
      assert.ok(result.usage.inputTokens > 0 && result.usage.outputTokens > 0, 'usage is reported');
      await assertRelayed(tap, [request(false, { contains: ['You are terse.', 'Say hello.'] })]);
    });

    test('streamText delivers the text in pieces', async () => {
      const result = streamText({ model: provider.model(), prompt: 'Say hello.', ...common });
      const pieces = [];
      for await (const piece of result.textStream) pieces.push(piece);
      assert.ok(pieces.length > 1, 'the text arrives in more than one piece');
      assert.equal(pieces.join(''), defaultReply);
      assert.equal(await result.finishReason, 'stop');
      assert.ok((await result.usage).outputTokens > 0, 'usage arrives with the stream');
      await assertRelayed(tap, [request(true, { contains: ['Say hello.'] })]);
    });

    test('an image arrives intact', async () => {
      const result = await generateText({
        model: provider.model(),
        messages: [{ role: 'user', content: [{ type: 'text', text: 'Describe the image.' }, { type: 'image', image: png, mediaType: 'image/png' }] }],
        maxRetries: 0,
        maxOutputTokens: 77
      });
      assert.equal(result.text, defaultReply);
      const { upstream } = await assertRelayed(tap, [{ api: provider.api(false), model: provider.slug, maxTokens: 77 }]);
      assert.deepEqual(images(provider.api(false), upstream[0].body), [{ mediaType: 'image/png', data: png.toString('base64') }]);
    });

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

    test('a tool loop completes and the tool schema arrives intact', async () => {
      const result = await generateText({ model: provider.model(), tools: { get_weather: weather }, stopWhen: stepCountIs(4), prompt: weatherPrompt, ...common });
      assert.equal(result.text, afterTools(`sunny in ${city}`));
      assert.equal(result.steps.length, 2);
      const [call] = result.steps[0].toolCalls;
      assert.equal(call.toolName, 'get_weather');
      assert.deepEqual(call.input, { city });
      assert.equal(result.steps[0].finishReason, 'tool-calls');
      const { upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
      assert.deepEqual(
        upstream.map((u) => u.script),
        ['tool_call', 'tool_result']
      );
    });

    test('a streamed tool loop assembles split tool arguments', async () => {
      const result = streamText({ model: provider.model(), tools: { get_weather: weather }, stopWhen: stepCountIs(4), prompt: weatherPrompt, ...common });
      const calls = [];
      let text = '';
      for await (const part of result.fullStream) {
        if (part.type === 'tool-call') calls.push({ name: part.toolName, input: part.input });
        if (part.type === 'text-delta') text += part.text;
      }
      assert.deepEqual(calls, [{ name: 'get_weather', input: { city } }]);
      assert.equal(text, afterTools(`sunny in ${city}`));
      await assertRelayed(tap, [request(true, { tools: ['get_weather'] }), request(true, { tools: ['get_weather'] })]);
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

    test('structured output returns the object the schema describes', async () => {
      const result = await generateText({
        model: provider.model(),
        output: Output.object({ schema: jsonSchema(ticketSchema) }),
        prompt: 'File a ticket.',
        ...common
      });
      assert.deepEqual(result.output, ticket);
      const { upstream } = await assertRelayed(tap, [request(false, { contains: ['File a ticket.'] })]);
      assert.equal(upstream[0].script, provider.structuredScript ?? 'structured', 'the upstream was asked for the schema');
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
