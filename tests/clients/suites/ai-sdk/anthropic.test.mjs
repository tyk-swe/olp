// Features of the Vercel AI SDK Anthropic provider that depend on the gateway
// passing Anthropic-specific request fields and signed content through
// untouched: prompt caching and extended thinking.
import assert from 'node:assert/strict';
import { after, beforeEach, test } from 'node:test';
import { generateText, stepCountIs } from 'ai';
import { afterTools, models, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed } from '../../lib/relay.mjs';
import { anthropic, city, tap, weather } from './providers.mjs';

after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

test('extended thinking is returned and replayed signed through a tool loop', async () => {
  const thinking = { anthropic: { thinking: { type: 'enabled', budgetTokens: 1024 } } };
  const prompt = `Weather in ${city}? ${script.think('The user wants the weather, so I should call the tool.')} ${script.tool('get_weather', { city })}`;
  const result = await generateText({ model: anthropic(models.anthropic), tools: { get_weather: weather }, stopWhen: stepCountIs(4), maxOutputTokens: 2048, maxRetries: 0, providerOptions: thinking, prompt });
  assert.equal(result.text, afterTools(`sunny in ${city}`));
  assert.equal(result.steps[0].reasoningText, 'The user wants the weather, so I should call the tool.');
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
