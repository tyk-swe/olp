import { runInNewContext } from 'node:vm';
import { describe, expect, it, vi } from 'vitest';
import { sdkSnippet } from './sdkSnippets';

describe('API key SDK snippets', () => {
  const secret = 'key "quoted"';
  const endpoint = 'https://gateway.example.test';
  const route = 'route "quoted"';

  it('configures and calls the Anthropic client with the supported revision', async () => {
    const construct = vi.fn();
    const create = vi.fn().mockResolvedValue({});
    class Anthropic {
      messages = { create };

      constructor(options: {
        apiKey: string;
        baseURL: string;
        defaultHeaders: Record<string, string>;
        maxRetries: number;
      }) {
        construct(options);
      }
    }
    const source = sdkSnippet(
      'anthropic',
      secret,
      `${endpoint}/`,
      route
    ).replace(/^import[^\n]+\n/, '');
    const run = runInNewContext(`(async (Anthropic) => { ${source} })`) as (
      client: typeof Anthropic
    ) => Promise<void>;
    await run(Anthropic);
    expect(construct).toHaveBeenCalledWith({
      apiKey: secret,
      baseURL: `${endpoint}/anthropic`,
      defaultHeaders: { 'anthropic-version': '2023-06-01' },
      maxRetries: 0
    });
    expect(create).toHaveBeenCalledWith({
      model: route,
      max_tokens: 512,
      messages: [{ role: 'user', content: 'Hello' }]
    });
  });

  it('configures and calls the Gemini client', async () => {
    const construct = vi.fn();
    const generateContent = vi.fn().mockResolvedValue({});
    class GoogleGenAI {
      models = { generateContent };

      constructor(options: {
        apiKey: string;
        httpOptions: { baseUrl: string; apiVersion: string };
      }) {
        construct(options);
      }
    }
    const source = sdkSnippet('gemini', secret, `${endpoint}/`, route).replace(
      /^import[^\n]+\n/,
      ''
    );
    const run = runInNewContext(`(async (GoogleGenAI) => { ${source} })`) as (
      client: typeof GoogleGenAI
    ) => Promise<void>;
    await run(GoogleGenAI);
    expect(construct).toHaveBeenCalledWith({
      apiKey: secret,
      httpOptions: { baseUrl: `${endpoint}/gemini`, apiVersion: 'v1beta' }
    });
    expect(generateContent).toHaveBeenCalledWith({
      model: route,
      contents: 'Hello'
    });
  });

  it('includes the selected credentials, endpoint and route in the OpenAI snippet', () => {
    const source = sdkSnippet('openai', secret, `${endpoint}/`, route);
    expect(source).toContain(JSON.stringify(secret));
    expect(source).toContain(JSON.stringify(`${endpoint}/v1`));
    expect(source).toContain(JSON.stringify(route));
  });
});
