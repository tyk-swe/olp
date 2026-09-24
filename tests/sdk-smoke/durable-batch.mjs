import assert from 'node:assert/strict';
import OpenAI, { toFile } from 'openai';

const origin = process.env.OLP_DURABLE_BASE;
const route = process.env.OLP_DURABLE_ROUTE;
const model = process.env.OLP_DURABLE_MODEL;
const client = new OpenAI({ baseURL: `${origin}/v1`, apiKey: process.env.OLP_DURABLE_KEY, maxRetries: 0, timeout: 15000 });
const input = [
  { custom_id: 'first', method: 'POST', url: '/v1/embeddings', body: { model, input: 'alpha' } },
  { custom_id: 'second', method: 'POST', url: '/v1/embeddings', body: { model, input: 'beta' } }
].map(item => JSON.stringify(item)).join('\n') + '\n';
const file = await toFile(Buffer.from(input), 'input.jsonl', { type: 'application/jsonl' });
const uploaded = await client.files.create({ file, purpose: 'batch' }, { headers: { 'X-OLP-Route': route } });
assert.match(uploaded.id, /^strict_file_/);
const created = await client.batches.create({ input_file_id: uploaded.id, endpoint: '/v1/embeddings', completion_window: '24h' });
assert.match(created.id, /^strict_batch_/);
assert.equal(created.input_file_id, uploaded.id);
const cancelled = await client.batches.cancel(created.id);
assert.equal(cancelled.status, 'cancelling');
const finished = await client.batches.retrieve(created.id);
assert.equal(finished.status, 'completed');
assert.equal(finished.request_counts.completed, 1);
assert.equal(finished.request_counts.failed, 1);
const output = await client.files.content(finished.output_file_id);
const errors = await client.files.content(finished.error_file_id);
assert.match(await output.text(), /"custom_id":"first"/);
assert.match(await errors.text(), /"custom_id":"second"/);
console.log('openai-js-7.4.0: strict durable batch lifecycle and partial files passed');
