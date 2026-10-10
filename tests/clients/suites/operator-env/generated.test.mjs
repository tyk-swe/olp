import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { beforeEach, test } from 'node:test';
import { generateText } from 'ai';
import {
  apiKey,
  assertClean,
  defaultReply,
  models,
  origin,
  recorded,
  resetRecorded,
  upstreamModels
} from '../../lib/harness.mjs';
import { namedModels } from '../../lib/relay.mjs';

beforeEach(resetRecorded);

for (const framework of ['openai-agents', 'vercel', 'langchain', 'llamaindex', 'sdk']) {
  for (const surface of framework === 'openai-agents'
    ? ['openai']
    : ['openai', 'anthropic', 'gemini']) {
    test(`generated ${framework}/${surface} configuration completes an authenticated request`, async () => {
      assert.ok(process.env.OLP_CLIENTS_OPERATOR_BINARY, 'run through tests/clients/run.sh');
      const parent = fileURLToPath(
        new URL(framework === 'sdk' ? '../../../sdk-smoke/' : '../../', import.meta.url)
      );
      const dir = await mkdtemp(join(parent, '.operator-env-'));
      try {
        const keyFile = join(dir, "key 'quoted'");
        await writeFile(keyFile, `${apiKey}\n`, { mode: 0o600 });
        const route =
          framework === 'openai-agents'
            ? models.openaiStrict
            : surface === 'openai'
              ? models.openai
              : namedModels[surface];
        const client = framework === 'sdk' ? surface : framework;
        const source = execFileSync(
          process.env.OLP_CLIENTS_OPERATOR_BINARY,
          [
            'client-env',
            client,
            '--format',
            'javascript',
            '--surface',
            surface,
            '--url',
            origin,
            '--key-file',
            keyFile,
            '--model',
            route
          ],
          { encoding: 'utf8' }
        );
        assert.ok(!source.includes(apiKey), 'generated source must not copy the key');
        const path = join(dir, 'config.mjs');
        await writeFile(path, source);
        const config = await import(pathToFileURL(path));
        const prompt = 'Say hello.';
        let answer;
        if (framework === 'openai-agents')
          answer = (await config.runner.run(config.agent, prompt)).finalOutput;
        else if (framework === 'vercel')
          answer = (
            await generateText({ model: config.model, prompt, maxRetries: 0, maxOutputTokens: 64 })
          ).text;
        else if (framework === 'langchain') answer = (await config.client.invoke(prompt)).content;
        else if (framework === 'llamaindex')
          answer = (await config.client.chat({ messages: [{ role: 'user', content: prompt }] }))
            .message.content;
        else if (surface === 'openai')
          answer = (
            await config.client.chat.completions.create({
              model: route,
              messages: [{ role: 'user', content: prompt }],
              max_completion_tokens: 64
            })
          ).choices[0].message.content;
        else if (surface === 'anthropic')
          answer = (
            await config.client.messages.create({
              model: route,
              max_tokens: 64,
              messages: [{ role: 'user', content: prompt }]
            })
          ).content[0].text;
        else
          answer = (await config.client.models.generateContent({ model: route, contents: prompt }))
            .text;
        if (Array.isArray(answer))
          answer = answer
            .map((part) => {
              assert.equal(part.type, 'text');
              return part.text;
            })
            .join('');
        assert.equal(answer, defaultReply);
        const requests = assertClean(await recorded());
        assert.equal(requests.length, 1);
        assert.equal(requests[0].model, upstreamModels[surface]);
      } finally {
        await rm(dir, { recursive: true, force: true });
      }
    });
  }
}
