// The LlamaIndex.TS LLMs against routes whose upstream speaks another vendor's
// API: the gateway translates each request and each reply. The client sees one
// answer, and the upstream must have received the same tools, the same
// generation parameters and the same conversation in its own dialect.
//
// The agent is not used here: it refuses a model name it does not recognize as
// able to call tools, and the routes of the other vendors are not named for
// the client's model. The tool loop is driven by hand instead.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { extractText } from 'llamaindex';
import { afterTools, models, recorded, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed, declaredForUpstream, toolDeclarations } from '../../lib/relay.mjs';
import { chatProviders, city, tap, weather } from './providers.mjs';

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

// The Gemini class answers a tool call with a functionResponse object that
// wraps the output as `result`, and the gateway hands the other vendors that
// object as the text of the tool result.
const toolResult = (provider) => (provider.api(false).startsWith('gemini') ? JSON.stringify({ result: `sunny in ${city}` }) : `sunny in ${city}`);

const weatherPrompt = `What is the weather in ${city}? ${script.tool('get_weather', { city })}`;
const messages = (user) => [
  { role: 'system', content: 'You are terse.' },
  { role: 'user', content: user }
];

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
        temperature: provider.sendsTemperature === false ? undefined : 0.2,
        contains: ['You are terse.'],
        ...extra
      });
      const create = () => provider.create({ temperature: 0.2, maxTokens: 77, slug: target.slug, ...provider.translationOptions });

      if (provider.translates === false) {
        test('a request with stored state is refused by name, before the upstream', async () => {
          const error = await create().chat({ messages: messages('Say hello.') }).then(
            () => assert.fail('the gateway translated a request it cannot preserve'),
            (rejection) => rejection
          );
          assert.equal(error.status, 400);
          assert.equal(error.code, 'unsupported_parameter');
          assert.match(error.message, /\/store/);
          assert.deepEqual(await recorded(), [], 'the upstream was never reached');
        });
        return;
      }

      test('a tool loop completes and the tools arrive as the client declared them', async () => {
        const llm = create();
        const conversation = messages(weatherPrompt);
        const first = await llm.chat({ messages: conversation, tools: [weather] });
        const [call] = first.message.options.toolCall;
        assert.deepEqual(typeof call.input === 'string' ? JSON.parse(call.input) : call.input, { city });
        const result = await weather.call({ city });
        const final = await llm.chat({
          messages: [...conversation, first.message, { role: 'user', content: result, options: { toolResult: { id: call.id, result, isError: false } } }],
          tools: [weather]
        });
        const { sent, upstream } = await assertRelayed(tap, [request(false, { tools: ['get_weather'] }), request(false, { tools: ['get_weather'] })]);
        // Whatever form the client declared the tool in, the upstream received the same name, description and schema.
        const [declared] = toolDeclarations(request(false).api, sent[0].body);
        assert.equal(declared.description, 'Look up the weather for a city');
        assert.deepEqual(toolDeclarations(request(false).to, upstream[0].body), [declaredForUpstream(request(false).api, declared)]);
        assert.ok(JSON.stringify(upstream[1].body).includes(`sunny in ${city}`), 'the tool result reaches the upstream');
        assert.equal(extractText(final.message.content), afterTools(toolResult(provider)));
      });

      if (provider.streams !== false) {
        test('a streamed chat completes', async () => {
          const pieces = [];
          for await (const chunk of await create().chat({ messages: messages('Say hello.'), stream: true })) pieces.push(chunk.delta);
          assert.equal(pieces.join(''), process.env.OLP_CLIENTS_DEFAULT_REPLY);
          await assertRelayed(tap, [request(true, { contains: ['You are terse.', 'Say hello.'] })]);
        });
      }
    });
  }
}
