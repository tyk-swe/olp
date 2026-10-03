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

const project = 'The shared project context that every request repeats. '.repeat(120);
const cached = { anthropic: { cacheControl: { type: 'ephemeral' } } };

test('a cache breakpoint writes the prefix, and the next request reads it', async () => {
  const ask = (question) =>
    generateText({
      model: anthropic(models.anthropic),
      maxOutputTokens: 64,
      maxRetries: 0,
      instructions: { role: 'system', content: project, providerOptions: cached },
      prompt: question
    });
  const first = await ask('First question.');
  const second = await ask('Second question.');

  const written = first.providerMetadata.anthropic.usage.cache_creation_input_tokens;
  assert.ok(written > 0, 'the first request writes the cache');
  assert.equal(first.usage.inputTokenDetails.cacheReadTokens, 0);
  assert.equal(second.usage.inputTokenDetails.cacheReadTokens, written, 'the second request reads what the first wrote');
  assert.equal(second.providerMetadata.anthropic.usage.cache_creation_input_tokens, 0);

  const { sent } = await assertRelayed(tap, [
    { api: 'anthropic.messages', model: models.anthropic, maxTokens: 64 },
    { api: 'anthropic.messages', model: models.anthropic, maxTokens: 64 }
  ]);
  // The breakpoint reached the upstream, which is what produced the cache usage.
  assert.equal(sent[0].body.system[0].cache_control.type, 'ephemeral');
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
