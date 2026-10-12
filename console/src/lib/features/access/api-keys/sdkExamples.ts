import { gatewayFetch } from '$lib/api/gateway';

import type { ApiKeySdk } from './sdkSnippets';
export { SDK_OPTIONS, sdkSnippet } from './sdkSnippets';
export type { ApiKeySdk } from './sdkSnippets';

export function sdkLabel(sdk: ApiKeySdk): string {
  if (sdk === 'openai') return 'OpenAI';
  if (sdk === 'anthropic') return 'Anthropic';
  return 'Gemini';
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
