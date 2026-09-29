import { gatewayFetch } from '$lib/api/gateway';

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
  return `from openai import OpenAI\n\nclient = OpenAI(\n    api_key="${secret}",\n    base_url="${endpoint}/v1",\n)\n\nresponse = client.responses.create(\n    model="${routeSlug}",\n    input="Hello",\n)`;
}

/** Sends the minimal request each SDK's snippet makes through the gateway,
 * authenticated by the new key. Throws the gateway's problem detail when the
 * route refuses it. */
export async function testSdkRequest(
  sdk: ApiKeySdk,
  secret: string,
  routeSlug: string
): Promise<void> {
  let response: Response;
  if (sdk === 'anthropic') {
    response = await gatewayFetch('/anthropic/v1/messages', {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'x-api-key': secret,
        'anthropic-version': '2023-06-01'
      },
      body: JSON.stringify({
        model: routeSlug,
        max_tokens: 16,
        messages: [{ role: 'user', content: 'Connection test' }]
      })
    });
  } else if (sdk === 'gemini') {
    response = await gatewayFetch(
      `/gemini/v1beta/models/${encodeURIComponent(routeSlug)}:generateContent`,
      {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-goog-api-key': secret
        },
        body: JSON.stringify({
          contents: [{ role: 'user', parts: [{ text: 'Connection test' }] }],
          generationConfig: { maxOutputTokens: 16 }
        })
      }
    );
  } else {
    response = await gatewayFetch('/v1/responses', {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        authorization: `Bearer ${secret}`
      },
      body: JSON.stringify({
        model: routeSlug,
        input: 'Connection test',
        max_output_tokens: 16
      })
    });
  }
  if (!response.ok) {
    let detail = `Request failed (${response.status}).`;
    try {
      const problem = (await response.json()) as {
        detail?: string;
        error?: { message?: string };
      };
      detail = problem.detail ?? problem.error?.message ?? detail;
    } catch {
      // The status remains enough when an intermediary returns no JSON.
    }
    throw new Error(detail);
  }
  await response.body?.cancel();
}
