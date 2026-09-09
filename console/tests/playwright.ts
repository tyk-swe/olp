import { test as base } from '@playwright/test';

export {
  expect,
  type APIRequestContext,
  type Locator,
  type Page,
  type Route,
  type TestInfo
} from '@playwright/test';

const svelteWarningMarkers = [
  'https://svelte.dev/e/',
  'Avoid using `history.pushState(...)` and `history.replaceState(...)`'
];

export const test = base.extend({
  page: async ({ page }, use) => {
    const runtimeFailures: string[] = [];
    page.on('pageerror', (error) => {
      runtimeFailures.push(`Uncaught page error: ${error.message}`);
    });
    page.on('console', (message) => {
      const text = message.text();
      if (
        (message.type() === 'error' &&
          !text.startsWith('Failed to load resource:')) ||
        (message.type() === 'warning' &&
          svelteWarningMarkers.some((marker) => text.includes(marker)))
      ) {
        runtimeFailures.push(`Console ${message.type()}: ${text}`);
      }
    });

    page.on('response', (response) => {
      const url = new URL(response.url());
      if (
        url.origin === 'http://localhost:4175' &&
        ['/api/', '/v1/', '/openai/', '/anthropic/', '/gemini/'].some(
          (prefix) => url.pathname.startsWith(prefix)
        ) &&
        response.headers()['cache-control'] !== 'no-store'
      ) {
        runtimeFailures.push(
          'A sensitive API response omitted Cache-Control: no-store'
        );
      }
    });

    await use(page);

    if (runtimeFailures.length > 0) {
      throw new Error(
        `Browser runtime failures:\n${runtimeFailures.join('\n')}`
      );
    }
  }
});
