import assert from 'node:assert/strict';
import { GoogleGenAI } from '@google/genai';

const baseUrl = process.env.OLP_GEMINI_BASE;
const apiKey = process.env.OLP_GEMINI_KEY;
const interactionRoute = process.env.OLP_GEMINI_INTERACTION_ROUTE;
const liveRoute = process.env.OLP_GEMINI_LIVE_ROUTE;
for (const value of [baseUrl, apiKey, interactionRoute, liveRoute]) assert.ok(value);

const client = new GoogleGenAI({ apiKey, httpOptions: { baseUrl, apiVersion: 'v1beta' } });
const options = { maxRetries: 0 };
const first = await client.interactions.create(
  {
    model: interactionRoute,
    input: 'hello',
    store: true,
    generation_config: { max_output_tokens: 64, seed: 0 },
    tools: [{ type: 'function', name: 'weather', parameters: { type: 'object' } }]
  },
  options
);
assert.match(first.id, /^interaction_/);
assert.equal(first.steps[0].signature, 'opaque-native');
assert.equal(first.steps[1].content[0].text, 'OK');

const second = await client.interactions.create(
  { model: interactionRoute, input: 'next', previous_interaction_id: first.id, store: true },
  options
);
assert.match(second.id, /^interaction_/);
assert.notEqual(second.id, first.id);

const retrieved = await client.interactions.get(first.id, {}, options);
assert.equal(retrieved.id, first.id);
assert.equal(retrieved.steps[0].content[0].text, 'retrieved');
const resumed = await client.interactions.get(first.id, { stream: true, last_event_id: 'cursor-start' }, options);
const resumedEvents = [];
for await (const event of resumed) resumedEvents.push(event);
assert.deepEqual(resumedEvents.map((event) => event.event_type), ['step.delta', 'step.stop', 'interaction.completed']);
assert.equal(resumedEvents[2].interaction.id, first.id);

const stream = await client.interactions.create(
  { model: interactionRoute, input: 'stream', store: true, stream: true },
  options
);
const events = [];
for await (const event of stream) events.push(event);
assert.deepEqual(
  events.map((event) => event.event_type),
  ['interaction.created', 'step.start', 'step.delta', 'step.stop', 'interaction.completed']
);
assert.match(events[0].interaction.id, /^interaction_/);
assert.equal(events[3].step.signature, 'opaque-native');
assert.equal(events[4].interaction.id, events[0].interaction.id);

const cancelled = await client.interactions.cancel(first.id, {}, options);
assert.equal(cancelled.id, first.id);
assert.equal(cancelled.status, 'cancelled');
await client.interactions.delete(first.id, {}, options);

// The pinned SDK appends /ws to a URL-serialized origin ending in /, producing
// //ws. The gateway's public /gemini base path is the qualified client shape.
const originClient = new GoogleGenAI({
  apiKey,
  httpOptions: { baseUrl: new URL(baseUrl).origin, apiVersion: 'v1beta' }
});
await assert.rejects(
  Promise.race([
    originClient.live.connect({ model: liveRoute, callbacks: {} }),
    new Promise((_, reject) => setTimeout(() => reject(new Error('double-slash upgrade timeout')), 4000))
  ])
);

let receive;
const audio = new Promise((resolve) => (receive = resolve));
const session = await client.live.connect({
  model: liveRoute,
  config: {
    responseModalities: ['AUDIO'],
    realtimeInputConfig: { automaticActivityDetection: { disabled: true } },
    sessionResumption: {}
  },
  callbacks: { onmessage(message) { if (message.serverContent) receive(message); } }
});
try {
  session.sendRealtimeInput({ activityStart: {} });
  session.sendRealtimeInput({ audio: { mimeType: 'audio/pcm;rate=16000', data: 'AQIDBA==' } });
  const response = await Promise.race([
    audio,
    new Promise((_, reject) => setTimeout(() => reject(new Error('Live audio timeout')), 5000))
  ]);
  assert.equal(response.serverContent.modelTurn.parts[0].inlineData.data, 'AQIDBA==');
  assert.equal(response.serverContent.interrupted, true);
  assert.equal(response.serverContent.turnComplete, true);
} finally {
  session.close();
}
console.log(JSON.stringify({ interaction: 'two-turn/stream/get/cancel/delete', live: 'audio/interruption', versions: '@google/genai@2.16.0' }));
