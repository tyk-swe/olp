import assert from 'node:assert/strict';
import OpenAI from 'openai';

const { OLP_CARRIER_ORIGIN: origin, OLP_CARRIER_KEY: key, OLP_CARRIER_ROUTE: route } = process.env;
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(key?.startsWith('olp_') && route);

for (const stream of [false, true]) {
  let submissions = 0;
  const client = new OpenAI({
    apiKey: key,
    baseURL: `${origin}/v1`,
    timeout: 15_000,
    fetch: (input, init) => {
      submissions++;
      return fetch(input, init);
    }
  });
  await assert.rejects(
    () => client.chat.completions.create({
      model: route,
      stream,
      messages: [{ role: 'user', content: 'carrier:fail' }]
    }),
    (error) => {
      assert.ok(error instanceof OpenAI.APIError);
      assert.equal(error.status, 502);
      assert.equal(error.code, 'ambiguous_upstream_result');
      return true;
    }
  );
  assert.equal(submissions, 1, `SDK repeated ambiguous carried work (stream=${stream})`);
}
