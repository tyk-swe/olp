import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  SDK_OPTIONS,
  sdkSnippet,
  testSdkRequest
} from '$lib/features/access/api-keys/sdkExamples';

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubGateway(response: Response) {
  const transport = vi.fn<typeof fetch>(async () => response);
  vi.stubGlobal('fetch', transport);
  return transport;
}

describe('sdkSnippet', () => {
  it.each(SDK_OPTIONS)(
    'uses the proxy endpoint, key, and public route for %s',
    (sdk) => {
      const snippet = sdkSnippet(
        sdk,
        'olp_test_secret',
        'https://proxy.example',
        'chat-route'
      );
      expect(snippet).toContain('olp_test_secret');
      expect(snippet).toContain('https://proxy.example');
      expect(snippet).toContain('chat-route');
    }
  );
});

describe('testSdkRequest', () => {
  it('posts the openai request with a bearer key', async () => {
    const transport = stubGateway(new Response('{}'));

    await testSdkRequest('openai', 'olp_secret', 'chat-route');

    const [input, init] = transport.mock.calls[0]!;
    expect(input).toBe('/v1/responses');
    expect(new Headers(init?.headers).get('authorization')).toBe(
      'Bearer olp_secret'
    );
    expect(init?.credentials).toBe('omit');
  });

  it('posts the anthropic request with an x-api-key', async () => {
    const transport = stubGateway(new Response('{}'));

    await testSdkRequest('anthropic', 'olp_secret', 'chat-route');

    const [input, init] = transport.mock.calls[0]!;
    expect(input).toBe('/anthropic/v1/messages');
    expect(new Headers(init?.headers).get('x-api-key')).toBe('olp_secret');
    expect(init?.credentials).toBe('omit');
  });

  it('posts the gemini request with the route slug and a goog key', async () => {
    const transport = stubGateway(new Response('{}'));

    await testSdkRequest('gemini', 'olp_secret', 'chat-route');

    const [input, init] = transport.mock.calls[0]!;
    expect(input).toBe('/gemini/v1beta/models/chat-route:generateContent');
    expect(new Headers(init?.headers).get('x-goog-api-key')).toBe('olp_secret');
    expect(init?.credentials).toBe('omit');
  });

  it('throws the problem detail of a refused request', async () => {
    stubGateway(
      new Response(JSON.stringify({ detail: 'The route refused the key.' }), {
        status: 403,
        headers: { 'content-type': 'application/problem+json' }
      })
    );

    await expect(
      testSdkRequest('openai', 'olp_secret', 'chat-route')
    ).rejects.toThrow('The route refused the key.');
  });

  it('throws the error message of a refused request', async () => {
    stubGateway(
      new Response(
        JSON.stringify({
          error: { message: 'The key cannot use this route.' }
        }),
        { status: 403, headers: { 'content-type': 'application/json' } }
      )
    );

    await expect(
      testSdkRequest('openai', 'olp_secret', 'chat-route')
    ).rejects.toThrow('The key cannot use this route.');
  });

  it('falls back to the status when the body is not JSON', async () => {
    stubGateway(
      new Response('<html>unavailable</html>', {
        status: 502,
        headers: { 'content-type': 'text/html' }
      })
    );

    await expect(
      testSdkRequest('openai', 'olp_secret', 'chat-route')
    ).rejects.toThrow('Request failed (502).');
  });
});
