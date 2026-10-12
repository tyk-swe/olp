export const SDK_OPTIONS = ['openai', 'anthropic', 'gemini'] as const;
export type ApiKeySdk = (typeof SDK_OPTIONS)[number];

export function sdkSnippet(
  sdk: ApiKeySdk,
  secret: string,
  endpoint: string,
  routeSlug: string
): string {
  const key = JSON.stringify(secret);
  const model = JSON.stringify(routeSlug);
  const origin = endpoint.replace(/\/+$/, '');
  if (sdk === 'anthropic') {
    return `import Anthropic from "@anthropic-ai/sdk";\n\nconst client = new Anthropic({\n  apiKey: ${key},\n  baseURL: ${JSON.stringify(`${origin}/anthropic`)},\n  maxRetries: 0,\n});\n\nconst message = await client.messages.create({\n  model: ${model},\n  max_tokens: 512,\n  messages: [{ role: "user", content: "Hello" }],\n});`;
  }
  if (sdk === 'gemini') {
    return `import { GoogleGenAI } from '@google/genai';\n\nconst ai = new GoogleGenAI({\n  apiKey: ${key},\n  httpOptions: {\n    baseUrl: ${JSON.stringify(`${origin}/gemini`)},\n    apiVersion: "v1beta",\n  },\n});\n\nconst response = await ai.models.generateContent({\n  model: ${model},\n  contents: "Hello",\n});`;
  }
  return `from openai import OpenAI\n\nclient = OpenAI(\n    api_key=${key},\n    base_url=${JSON.stringify(`${origin}/v1`)},\n    max_retries=0,\n)\n\nresponse = client.responses.create(\n    model=${model},\n    input="Hello",\n)`;
}
