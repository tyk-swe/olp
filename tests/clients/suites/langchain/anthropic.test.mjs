// Features of ChatAnthropic that depend on the gateway passing Anthropic fields
// and signed content through untouched: prompt caching and extended thinking.
import assert from 'node:assert/strict';
import { after, beforeEach, test } from 'node:test';
import { HumanMessage } from '@langchain/core/messages';
import { afterTools, models, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed, textOf } from '../../lib/relay.mjs';
import { chatProviders, city, tap, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const anthropic = chatProviders.find((provider) => provider.name === 'ChatAnthropic');
const project = 'The shared project context that every request repeats. '.repeat(120);

test('a cache breakpoint writes the prefix, and the next request reads it', async () => {
  const model = anthropic.create({ maxTokens: 64 });
  const ask = (question) =>
    model.invoke([new HumanMessage({ content: [{ type: 'text', text: project, cache_control: { type: 'ephemeral' } }, { type: 'text', text: question }] })]);
  const first = await ask('First question.');
  const second = await ask('Second question.');
  const written = first.usage_metadata.input_token_details.cache_creation;
  assert.ok(written > 0, 'the first request writes the cache');
  assert.equal(second.usage_metadata.input_token_details.cache_read, written, 'the second request reads what the first wrote');
  const { sent } = await assertRelayed(tap, [
    { api: 'anthropic.messages', model: models.anthropic, maxTokens: 64 },
    { api: 'anthropic.messages', model: models.anthropic, maxTokens: 64 }
  ]);
  assert.equal(sent[0].body.messages[0].content[0].cache_control.type, 'ephemeral');
});

test('extended thinking is returned and replayed signed through a tool loop', async () => {
  const model = anthropic.create({ maxTokens: 2048, thinking: { type: 'enabled', budget_tokens: 1024 } }).bindTools([weather]);
  const prompt = `Weather in ${city}? ${script.think('The user wants the weather, so I should call the tool.')} ${script.tool('get_weather', { city })}`;
  const messages = [new HumanMessage(prompt)];
  const call = await model.invoke(messages);
  const thinking = call.content.find((block) => block.type === 'thinking');
  assert.equal(thinking.thinking, 'The user wants the weather, so I should call the tool.');
  assert.ok(thinking.signature, 'the thinking block is signed');
  const result = await weather.invoke(call.tool_calls[0]);
  const final = await model.invoke([...messages, call, result]);
  assert.equal(textOf(final.content), afterTools(`sunny in ${city}`));
  // The upstream rejects a history whose thinking signature was altered, so a
  // second answered request proves the signed block survived the round trip.
  const { sent, upstream } = await assertRelayed(tap, [
    { api: 'anthropic.messages', model: models.anthropic },
    { api: 'anthropic.messages', model: models.anthropic }
  ]);
  assert.equal(sent[0].body.thinking.budget_tokens, 1024);
  const replayed = upstream[1].body.messages.find((message) => message.role === 'assistant').content;
  assert.deepEqual(
    replayed.map((block) => block.type),
    ['thinking', 'tool_use']
  );
  assert.ok(replayed[0].signature, 'the thinking block keeps its signature');
});
