// The Vercel AI SDK providers against routes whose upstream speaks another
// vendor's API: the gateway translates each request and each reply. The client
// sees one answer, and the upstream must have received the same tools, the same
// generation parameters and the same conversation in its own dialect.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { Output, generateText, jsonSchema, stepCountIs, streamText } from 'ai';
import { afterTools, defaultReply, models, recorded, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed, declaredForUpstream, images, png, toolDeclarations } from '../../lib/relay.mjs';
import { chatProviders, city, tap, ticket, ticketSchema, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

// The API the gateway uses for each upstream. Only Anthropic and Gemini clients
// reach an OpenAI upstream here, and the gateway speaks Chat Completions to it.
const targets = [
  { name: 'an OpenAI upstream', slug: models.openai, vendor: 'openai', to: () => 'openai.chat' },
  { name: 'an Anthropic upstream', slug: models.anthropic, vendor: 'anthropic', to: () => 'anthropic.messages' },
  { name: 'a Gemini upstream', slug: models.gemini, vendor: 'gemini', to: (_, stream) => (stream ? 'gemini.stream' : 'gemini.generate') }
];

// The Google provider answers a tool call with a functionResponse object that
// names the tool and carries the output, and the gateway hands the other
// vendors that object as the text of the tool result.
const toolResult = (provider) => (provider.api(false).startsWith('gemini') ? JSON.stringify({ name: 'get_weather', content: `sunny in ${city}` }) : `sunny in ${city}`);

const refusedStructured = (provider, target) => provider.api(false).startsWith('anthropic') && target.vendor === 'gemini';

const common = { maxRetries: 0, maxOutputTokens: 77, temperature: 0.2, instructions: 'You are terse.' };
const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;

for (const provider of chatProviders) {
  const native = provider.api(false).split('.')[0];
  for (const target of targets.filter((t) => t.vendor !== native)) {
    describe(`${provider.name} client, ${target.name}`, () => {
      const request = (stream, extra = {}) => ({
        api: provider.api(stream),
        to: target.to(provider.api(stream), stream),
        model: target.slug,
        stream,
        maxTokens: 77,
        temperature: 0.2,
        contains: ['You are terse.'],
        ...extra
      });
      const model = () => provider.model(undefined, target.slug);

      test('a tool loop completes and the tools arrive as the client declared them', async () => {
        const result = await generateText({ model: model(), tools: { get_weather: weather }, stopWhen: stepCountIs(4), prompt: weatherPrompt, ...common });
        assert.equal(result.text, afterTools(toolResult(provider)));
        assert.deepEqual(
          result.steps[0].toolCalls.map((call) => call.input),
          [{ city }]
        );
        const { sent, upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
        // Whatever form the client declared the tool in, the upstream received the same name, description and schema.
        const [declared] = toolDeclarations(request(false).api, sent[0].body);
        assert.equal(declared.description, 'Look up the weather for a city');
        assert.deepEqual(toolDeclarations(request(false).to, upstream[0].body), [declaredForUpstream(request(false).api, declared)]);
        assert.ok(JSON.stringify(upstream[1].body).includes(`sunny in ${city}`), 'the tool result reaches the upstream');
      });

      test('an image arrives intact in the upstream dialect', async () => {
        const result = await generateText({
          model: model(),
          messages: [{ role: 'user', content: [{ type: 'text', text: 'Describe the image.' }, { type: 'image', image: png, mediaType: 'image/png' }] }],
          ...common
        });
        assert.equal(result.text, defaultReply);
        const { upstream } = await assertRelayed(tap, [request(false)]);
        assert.deepEqual(images(request(false).to, upstream[0].body), [{ mediaType: 'image/png', data: png.toString('base64') }]);
      });

      test('a streamed tool loop completes', async () => {
        const result = streamText({ model: model(), tools: { get_weather: weather }, stopWhen: stepCountIs(4), prompt: weatherPrompt, ...common });
        let text = '';
        for await (const part of result.fullStream) if (part.type === 'text-delta') text += part.text;
        assert.equal(text, afterTools(toolResult(provider)));
        await assertRelayed(tap, [request(true, { tools: ['get_weather'] }), request(true, { tools: ['get_weather'] })]);
      });

      test('structured output returns the object the schema describes', async () => {
        const ask = () => generateText({ model: model(), output: Output.object({ schema: jsonSchema(ticketSchema) }), prompt: 'File a ticket.', ...common });
        if (refusedStructured(provider, target)) {
          // The Anthropic provider asks for JSON through a forced tool call with
          // parallel tool use disabled, which Gemini cannot preserve. The gateway
          // refuses it by name rather than dropping the restriction.
          const error = await ask().then(
            () => assert.fail('the gateway translated a request it cannot preserve'),
            (rejection) => rejection
          );
          assert.equal(error.statusCode, 400);
          assert.match(error.message, /parallel_tool_calls/);
          assert.deepEqual(await recorded(), [], 'the upstream was never reached');
          return;
        }
        const result = await ask();
        assert.deepEqual(result.output, ticket);
        await assertRelayed(tap, [request(false, { contains: ['File a ticket.'] })]);
      });
    });
  }
}
