import assert from 'node:assert/strict';
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: `${process.env.OLP_RESPONSES_BASE}/v1`,
  apiKey: process.env.OLP_RESPONSES_KEY,
  maxRetries: 0,
  timeout: 15000
});

const created = await client.responses.create({
  model: process.env.OLP_RESPONSES_ROUTE,
  input: 'Retained native stream through the public SDK',
  background: true,
  store: true,
  stream: true
});
let local = '';
for await (const event of created) {
  if (event.type === 'response.created') {
    local = event.response.id;
    break; // SDK aborts delivery; accepted provider work remains recoverable.
  }
}
assert.match(local, /^strict_response_/);

const unary = await client.responses.retrieve(local);
assert.equal(unary.id, local);
assert.equal(unary.status, 'completed');

const resumed = await client.responses.retrieve(local, {
  stream: true,
  starting_after: 0,
  include: ['reasoning.encrypted_content'],
  include_obfuscation: false
});
let completed = false;
for await (const event of resumed) {
  if (event.type === 'response.completed') {
    assert.equal(event.response.id, local);
    completed = true;
  }
}
assert.ok(completed, 'resumed stream has no terminal native event');
console.log(JSON.stringify({ id: local, resumed: completed }));
