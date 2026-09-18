import assert from 'node:assert/strict';
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: `${process.env.OLP_RECOVERY_ORIGIN}/v1`,
  apiKey: process.env.OLP_RECOVERY_API_KEY,
  maxRetries: 0,
  timeout: 20_000
});
const response = await client.responses.create({
  model: process.env.OLP_RECOVERY_MODEL,
  input: 'Connection test',
  max_output_tokens: 16
});
assert.equal(response.output_text, process.env.OLP_RECOVERY_REPLY);
console.log('Official OpenAI SDK passed against the restored installation.');
