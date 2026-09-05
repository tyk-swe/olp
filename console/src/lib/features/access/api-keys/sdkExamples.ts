export const SDK_OPTIONS = ['openai', 'anthropic', 'gemini'] as const;
export type ApiKeySdk = (typeof SDK_OPTIONS)[number];

export function sdkLabel(sdk: ApiKeySdk): string {
  if (sdk === 'openai') return 'OpenAI';
  if (sdk === 'anthropic') return 'Anthropic';
  return 'Gemini';
}

export function sdkSnippet(
  sdk: ApiKeySdk,
  secret: string,
  endpoint: string,
  routeSlug: string
): string {
  if (sdk === 'anthropic') {
    return `import Anthropic from "@anthropic-ai/sdk";\n\nconst client = new Anthropic({\n  apiKey: "${secret}",\n  baseURL: "${endpoint}/anthropic",\n});\n\nconst message = await client.messages.create({\n  model: "${routeSlug}",\n  max_tokens: 512,\n  messages: [{ role: "user", content: "Hello" }],\n});`;
  }
  if (sdk === 'gemini') {
    return `import { GoogleGenAI } from '@google/genai';\n\nconst ai = new GoogleGenAI({\n  apiKey: "${secret}",\n  apiVersion: "v1beta",\n  httpOptions: {\n    baseUrl: "${endpoint}/gemini",\n    apiVersion: "v1beta",\n    retryOptions: { attempts: 1 },\n  },\n});\n\nconst response = await ai.models.generateContent({\n  model: "${routeSlug}",\n  contents: "Hello",\n});`;
  }
  return `from openai import OpenAI\n\nclient = OpenAI(\n    api_key="${secret}",\n    base_url="${endpoint}/openai/v1",\n)\n\nresponse = client.responses.create(\n    model="${routeSlug}",\n    input="Hello",\n)`;
}
