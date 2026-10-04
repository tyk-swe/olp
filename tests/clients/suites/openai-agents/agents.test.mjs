// OpenAI Agents SDK through OLP's Responses surface: function tools, handoffs
// between agents, structured outputs and streaming. The SDK is pointed at the
// gateway with its documented OpenAIProvider, so these are the requests an
// operator's agents send.
import assert from 'node:assert/strict';
import { after, beforeEach, describe, test } from 'node:test';
import { Agent, OpenAIProvider, Runner, handoff, setTracingDisabled, tool } from '@openai/agents';
import { z } from 'zod';
import { afterTools, apiKey, models, recorded, resetRecorded, script } from '../../lib/harness.mjs';
import { assertRelayed, toolDeclarations } from '../../lib/relay.mjs';
import { startTap } from '../../lib/tap.mjs';

// The SDK exports traces to api.openai.com; a run here must not try.
setTracingDisabled(true);

const tap = await startTap();
after(() => tap.close());
beforeEach(async () => {
  await resetRecorded();
  tap.reset();
});

const runnerFor = (useResponses) =>
  new Runner({ modelProvider: new OpenAIProvider({ apiKey, baseURL: tap.baseURLs.openai, useResponses }), tracingDisabled: true });
const runner = runnerFor(true);

const city = 'Zürich 東京 🚀';
const weatherSchema = {
  type: 'object',
  properties: { city: { type: 'string', description: 'The city to look up', minLength: 2 } },
  required: ['city'],
  additionalProperties: false
};
// A JSON Schema given as is, so what the upstream receives is what this says.
const getWeather = tool({
  name: 'get_weather',
  description: 'Look up the weather for a city',
  parameters: weatherSchema,
  strict: false,
  execute: async ({ city }) => `sunny in ${city}`
});

const request = (extra = {}) => ({ api: 'openai.responses', model: models.openai, ...extra });

describe('Responses API', () => {
  test('a run completes a plain answer', async () => {
    const agent = new Agent({ name: 'Assistant', instructions: 'You are terse.', model: models.openai });
    const result = await runner.run(agent, 'Say hello.');
    assert.equal(result.finalOutput, process.env.OLP_CLIENTS_DEFAULT_REPLY);
    assert.ok(result.rawResponses[0].usage.totalTokens > 0, 'usage is reported');
    await assertRelayed(tap, [request({ contains: ['You are terse.', 'Say hello.'] })]);
  });

  test('a function tool loop completes and the schema arrives intact', async () => {
    const agent = new Agent({ name: 'Weather', instructions: 'Use the tool.', model: models.openai, tools: [getWeather] });
    const result = await runner.run(agent, `Weather in ${city}? ${script.tool('get_weather', { city })}`);
    assert.equal(result.finalOutput, afterTools(`sunny in ${city}`));
    assert.equal(result.rawResponses.length, 2);
    const { upstream } = await assertRelayed(tap, [request({ tools: ['get_weather'] }), request({ tools: ['get_weather'] })]);
    assert.deepEqual(toolDeclarations('openai.responses', upstream[0].body)[0].schema, weatherSchema);
    assert.deepEqual(
      upstream.map((u) => u.script),
      ['tool_call', 'tool_result']
    );
    const output = upstream[1].body.input.find((item) => item.type === 'function_call_output');
    assert.equal(output.output, `sunny in ${city}`, 'the tool result reaches the upstream');
  });

  test('a streamed run assembles text and tool calls', async () => {
    const agent = new Agent({ name: 'Weather', instructions: 'Use the tool.', model: models.openai, tools: [getWeather] });
    const result = await runner.run(agent, `Weather in ${city}? ${script.tool('get_weather', { city })}`, { stream: true });
    let text = '';
    for await (const piece of result.toTextStream()) text += piece;
    await result.completed;
    assert.equal(text, afterTools(`sunny in ${city}`));
    assert.equal(result.finalOutput, text);
    const calls = result.newItems.filter((item) => item.type === 'tool_call_item');
    assert.deepEqual(
      calls.map((item) => JSON.parse(item.rawItem.arguments)),
      [{ city }]
    );
    await assertRelayed(tap, [request({ stream: true, tools: ['get_weather'] }), request({ stream: true, tools: ['get_weather'] })]);
  });

  test('a handoff passes the conversation to a second agent that has its own tools and instructions', async () => {
    const lookupInvoice = tool({
      name: 'lookup_invoice',
      description: 'Find an invoice by number',
      parameters: { type: 'object', properties: { number: { type: 'string' } }, required: ['number'], additionalProperties: false },
      strict: false,
      execute: async ({ number }) => `invoice ${number} is paid`
    });
    const billing = new Agent({ name: 'Billing', instructions: 'You handle invoices.', model: models.openai, tools: [lookupInvoice], handoffDescription: 'Billing questions' });
    const transfer = handoff(billing);
    const triage = new Agent({ name: 'Triage', instructions: 'Route the question.', model: models.openai, handoffs: [transfer] });

    const prompt = `Is invoice INV-7 paid? ${script.tool(transfer.toolName, {})} ${script.tool('lookup_invoice', { number: 'INV-7' })}`;
    const result = await runner.run(triage, prompt);
    assert.equal(result.lastAgent.name, 'Billing', 'the second agent finished the run');

    const { upstream } = await assertRelayed(tap, [
      request({ tools: [transfer.toolName], contains: ['Route the question.'] }),
      request({ tools: ['lookup_invoice'], contains: ['You handle invoices.'] }),
      request({ tools: ['lookup_invoice'], contains: ['You handle invoices.'] })
    ]);
    assert.ok(!JSON.stringify(upstream[1].body).includes('Route the question.'), 'the second agent has its own instructions');
    const handoffCall = upstream[1].body.input.find((item) => item.type === 'function_call' && item.name === transfer.toolName);
    assert.ok(handoffCall, 'the handoff call is part of the history the second agent sees');
    const handoffResult = upstream[2].body.input.find((item) => item.type === 'function_call_output' && item.call_id === handoffCall.call_id);
    assert.ok(handoffResult, 'the handoff result is part of the history the second agent sees');
    assert.equal(result.finalOutput, afterTools(handoffResult.output, 'invoice INV-7 is paid'));
  });

  test('structured output returns the object the schema describes', async () => {
    const Ticket = z.object({ title: z.string(), priority: z.enum(['low', 'high']), score: z.number().int(), done: z.boolean(), tags: z.array(z.string()) });
    const agent = new Agent({ name: 'Filer', instructions: 'File a ticket.', model: models.openai, outputType: Ticket });
    const result = await runner.run(agent, 'The printer is on fire.');
    assert.deepEqual(result.finalOutput, { title: 'fixture', priority: 'low', score: 42, done: true, tags: ['fixture'] });
    const { sent, upstream } = await assertRelayed(tap, [request()]);
    assert.equal(upstream[0].script, 'structured');
    assert.equal(upstream[0].body.text.format.type, 'json_schema');
    assert.deepEqual(upstream[0].body.text.format.schema, sent[0].body.text.format.schema, 'the output schema reaches the upstream unchanged');
  });

  test('a reasoning model run replays its reasoning items through a tool loop', async () => {
    const agent = new Agent({
      name: 'Thinker',
      instructions: 'Think first.',
      model: models.openai,
      tools: [getWeather],
      modelSettings: { reasoning: { effort: 'low', summary: 'auto' }, providerData: { include: ['reasoning.encrypted_content'] } }
    });
    const result = await runner.run(agent, `Weather in ${city}? ${script.tool('get_weather', { city })}`);
    assert.equal(result.finalOutput, afterTools(`sunny in ${city}`));
    assert.ok(result.newItems.some((item) => item.type === 'reasoning_item'), 'the run kept the reasoning item');
    const { upstream } = await assertRelayed(tap, [request({ tools: ['get_weather'] }), request({ tools: ['get_weather'] })]);
    assert.deepEqual(upstream[0].body.reasoning, { effort: 'low', summary: 'auto' });
    const replayed = upstream[1].body.input.find((item) => item.type === 'reasoning');
    assert.equal(replayed.encrypted_content, 'fixture-encrypted-reasoning', 'the reasoning item comes back to the upstream with its encrypted content');
  });

  test('a strict route works when the run asks for no stored state', async () => {
    const agent = new Agent({ name: 'Assistant', instructions: 'You are terse.', model: models.openaiStrict, modelSettings: { store: false } });
    const result = await runner.run(agent, 'Say hello.');
    assert.equal(result.finalOutput, process.env.OLP_CLIENTS_DEFAULT_REPLY);
    const { upstream } = await assertRelayed(tap, [request({ model: models.openaiStrict })]);
    assert.equal(upstream[0].body.store, false);
  });
});

describe('Chat Completions API', () => {
  test('a tool loop completes through the chat completions model', async () => {
    const agent = new Agent({ name: 'Weather', instructions: 'Use the tool.', model: models.openai, tools: [getWeather] });
    const result = await runnerFor(false).run(agent, `Weather in ${city}? ${script.tool('get_weather', { city })}`);
    assert.equal(result.finalOutput, afterTools(`sunny in ${city}`));
    const chat = { api: 'openai.chat', model: models.openai, tools: ['get_weather'] };
    await assertRelayed(tap, [chat, chat]);
  });
});

describe('another vendor behind the route', () => {
  test('a strict tool, the default, is refused by name before the upstream', async () => {
    // OpenAI's strict mode has no equivalent at Anthropic or Gemini, so the gateway will not drop it.
    const strict = tool({ name: 'get_weather', description: 'Look up the weather', parameters: z.object({ city: z.string() }), execute: async () => 'sunny' });
    const agent = new Agent({ name: 'Weather', instructions: 'Use the tool.', model: models.anthropic, tools: [strict] });
    const error = await runner.run(agent, 'Weather?').then(
      () => assert.fail('the gateway translated a request it cannot preserve'),
      (rejection) => rejection
    );
    assert.equal(error.status, 400);
    assert.equal(error.code, 'unsupported_parameter');
    assert.match(error.message, /\/tools\/0\/strict/);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });

  test('an Anthropic upstream refuses a run that sets no token limit, naming it, before the upstream', async () => {
    // Anthropic requires a limit and the gateway will not invent one; a provider default or modelSettings.maxTokens supplies it.
    const agent = new Agent({ name: 'Assistant', instructions: 'You are terse.', model: models.anthropic });
    const error = await runner.run(agent, 'Say hello.').then(
      () => assert.fail('the gateway invented a token limit'),
      (rejection) => rejection
    );
    assert.equal(error.status, 400);
    assert.equal(error.code, 'unsupported_parameter');
    assert.match(error.message, /max_output_tokens/);
    assert.deepEqual(await recorded(), [], 'the upstream was never reached');
  });

  for (const [vendor, slug, to] of [
    ['Anthropic', models.anthropic, 'anthropic.messages'],
    ['Gemini', models.gemini, 'gemini.generate']
  ]) {
    test(`a tool loop reaches ${vendor} by translation`, async () => {
      // The SDK sends no token limit, which Anthropic requires and the gateway will not invent.
      const agent = new Agent({ name: 'Weather', instructions: 'Use the tool.', model: slug, tools: [getWeather], modelSettings: { maxTokens: 64 } });
      const result = await runner.run(agent, `Weather in ${city}? ${script.tool('get_weather', { city })}`);
      assert.equal(result.finalOutput, afterTools(`sunny in ${city}`));
      const limited = { model: slug, to, tools: ['get_weather'], maxTokens: 64 };
      const { upstream } = await assertRelayed(tap, [request(limited), request(limited)]);
      assert.deepEqual(toolDeclarations(to, upstream[0].body)[0].schema, weatherSchema);
    });
  }
});
