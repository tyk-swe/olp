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
import { models, recorded, resetRecorded } from '../../lib/harness.mjs';
import { chatProviders, tap } from './providers.mjs';

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
const messages = (user) => [
  { role: 'system', content: 'You are terse.' },
  { role: 'user', content: user }
];

for (const provider of chatProviders) {
  const native = provider.api(false).split('.')[0];
  for (const target of targets.filter((t) => t.vendor !== native)) {
    describe(`${provider.name} client, ${target.name}`, () => {
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
    });
  }
}
