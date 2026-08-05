import { describe, expect, it } from 'vitest';
import { SDK_OPTIONS, sdkSnippet } from './sdkExamples';

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
