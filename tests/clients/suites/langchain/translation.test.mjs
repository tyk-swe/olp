// The LangChain chat models against routes whose upstream speaks another
// vendor's API: the gateway translates each request and each reply. The client
// sees one answer, and the upstream must have received the same tools, the same
// generation parameters and the same conversation in its own dialect.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { HumanMessage, SystemMessage } from '@langchain/core/messages';
import { afterTools, defaultReply, models, recorded, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed, declaredForUpstream, images, png, textOf, toolDeclarations } from '../../lib/relay.mjs';
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

// The Google model answers a tool call with a functionResponse object that
// wraps the output as `result`, and the gateway hands the other vendors that
// object as the text of the tool result.
const toolResult = (provider) => (provider.api(false).startsWith('gemini') ? JSON.stringify({ result: `sunny in ${city}` }) : `sunny in ${city}`);

const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;
const terse = new SystemMessage('You are terse.');

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
      const create = () => provider.create({ temperature: 0.2, maxTokens: 77, slug: target.slug });

      test('a tool loop completes and the tools arrive as the client declared them', async () => {
        const model = create().bindTools([weather]);
        const messages = [terse, new HumanMessage(weatherPrompt)];
        const call = await model.invoke(messages);
        assert.deepEqual(call.tool_calls[0].args, { city });
        const result = await weather.invoke(call.tool_calls[0]);
        const final = await model.invoke([...messages, call, result]);
        assert.equal(textOf(final.content), afterTools(toolResult(provider)));
        const { sent, upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
        // Whatever form the client declared the tool in, the upstream received the same name, description and schema.
        const [declared] = toolDeclarations(request(false).api, sent[0].body);
        assert.equal(declared.description, 'Look up the weather for a city');
        assert.deepEqual(toolDeclarations(request(false).to, upstream[0].body), [declaredForUpstream(request(false).api, declared)]);
        assert.ok(JSON.stringify(upstream[1].body).includes(`sunny in ${city}`), 'the tool result reaches the upstream');
      });

      // Unqualifiable where the client decides from the model name whether it takes images.
      if (!provider.imagesNeedKnownModelName) {
        test('an image arrives intact in the upstream dialect', async () => {
          const content = [{ type: 'text', text: 'Describe the image.' }, { type: 'image_url', image_url: { url: `data:image/png;base64,${png.toString('base64')}` } }];
          const reply = await create().invoke([terse, new HumanMessage({ content })]);
          assert.equal(textOf(reply.content), defaultReply);
          const { upstream } = await assertRelayed(tap, [request(false)]);
          assert.deepEqual(images(request(false).to, upstream[0].body), [{ mediaType: 'image/png', data: png.toString('base64') }]);
        });
      }

      test('a streamed tool call is assembled from its fragments', async () => {
        let call;
        for await (const chunk of await create().bindTools([weather]).stream([terse, new HumanMessage(weatherPrompt)])) call = call ? call.concat(chunk) : chunk;
        assert.deepEqual(call.tool_calls[0].args, { city });
        await assertRelayed(tap, [request(true, { tools: ['get_weather'] })]);
      });

      test('structured output returns the object the schema describes', async () => {
        const result = await create().withStructuredOutput(ticketSchema).invoke([terse, new HumanMessage('File a ticket.')]);
        assert.deepEqual(result, ticket);
        await assertRelayed(tap, [request(false, { contains: ['File a ticket.'] })]);
      });
    });
  }
}
