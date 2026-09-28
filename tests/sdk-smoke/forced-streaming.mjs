import assert from 'node:assert/strict';
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: `${process.env.OLP_RESPONSES_BASE}/v1`,
  apiKey: process.env.OLP_RESPONSES_KEY,
  maxRetries: 0,
  timeout: 15000
});

// A non-streaming call through a plugin profile whose upstream serves only
// streams: the gateway aggregates the upstream's stream into the result.
const response = await client.responses.create({
  model: process.env.OLP_RESPONSES_ROUTE,
  input: 'Reply through an upstream that serves only streams'
});
assert.equal(response.object, 'response');
assert.equal(response.status, 'completed');
assert.equal(response.model, process.env.OLP_RESPONSES_ROUTE);
assert.equal(response.output_text, process.env.OLP_RESPONSES_EXPECT);
assert.equal(response.usage.total_tokens, 12);
console.log(JSON.stringify({ id: response.id, output_text: response.output_text }));
