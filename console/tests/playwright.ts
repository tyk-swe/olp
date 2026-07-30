import { test as base, type Page, type Route } from '@playwright/test';

export {
  expect,
  type APIRequestContext,
  type Locator,
  type Page,
  type Route
} from '@playwright/test';

export function failUnexpectedApiRequest(route: Route): never {
  const request = route.request();
  throw new Error(`Unexpected API request: ${request.method()} ${new URL(request.url()).pathname}`);
}

export async function denyClipboard(page: Page) {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText() {
          throw new DOMException('Clipboard permission denied', 'NotAllowedError');
        }
      }
    });
  });
}

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
        (message.type() === 'error' && !text.startsWith('Failed to load resource:'))
        || (
          message.type() === 'warning'
          && svelteWarningMarkers.some((marker) => text.includes(marker))
        )
      ) {
        runtimeFailures.push(`Console ${message.type()}: ${text}`);
      }
    });

    await use(page);

    if (runtimeFailures.length > 0) {
      throw new Error(`Browser runtime failures:\n${runtimeFailures.join('\n')}`);
    }
  }
});
