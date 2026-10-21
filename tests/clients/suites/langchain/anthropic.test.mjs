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
