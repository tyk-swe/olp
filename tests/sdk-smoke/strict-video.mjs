import assert from 'node:assert/strict';
import OpenAI, { toFile } from 'openai';

const client = new OpenAI({
  baseURL: `${process.env.OLP_VIDEO_BASE}/v1`,
  apiKey: process.env.OLP_VIDEO_KEY,
  maxRetries: 0,
  timeout: 15000
});
const reference = await toFile(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0, 1, 0xff]), 'first-frame.png', {
  type: 'image/png'
});
const created = await client.videos.create({
  prompt: 'one frame, eight seconds',
  input_reference: reference,
  model: process.env.OLP_VIDEO_ROUTE,
  seconds: '8',
  size: '1280x720'
});
assert.match(created.id, /^[a-f0-9-]{36}$/);
assert.equal(created.status, 'queued');
assert.equal(created.model, process.env.OLP_VIDEO_ROUTE);
const completed = await client.videos.retrieve(created.id);
assert.equal(completed.id, created.id);
assert.equal(completed.status, 'completed');
const video = await client.videos.downloadContent(created.id);
assert.deepEqual(Buffer.from(await video.arrayBuffer()), Buffer.from([0, 0, 0, 8, 109, 111, 111, 118]));
assert.equal(video.headers.get('content-type'), 'video/mp4; codecs="avc1.42E01E"');
const thumbnail = await client.videos.downloadContent(created.id, { variant: 'thumbnail' });
assert.deepEqual(Buffer.from(await thumbnail.arrayBuffer()), Buffer.from([0xff, 0xd8, 0, 1, 0xff, 0xd9]));
const deleted = await client.videos.delete(created.id);
assert.equal(deleted.id, created.id);
assert.equal(deleted.deleted, true);
console.log('openai-js-7.4.0: strict video create, retrieve, binary content and delete passed');
