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
  throw new Error(
    `Unexpected API request: ${request.method()} ${new URL(request.url()).pathname}`
  );
}

export async function mockSession(
  page: Page,
  {
    userId = '01980000-0000-7000-8000-000000000001',
    csrfToken = 'csrf-test-token'
  }: { userId?: string; csrfToken?: string } = {}
) {
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: {
          id: userId,
          email: 'owner@example.com',
          display_name: 'Ada Owner',
          role: 'owner'
        },
        csrf_token: csrfToken
      }
    });
  });
}

export async function emulateTwoHundredPercentZoom(page: Page) {
  const viewport = page.viewportSize();
  // Mobile projects already exercise the narrow layout; resizing an emulated
  // device desynchronizes Chromium's visual and layout viewports.
  if (!viewport || viewport.width <= 480) return;
  // Halving the CSS-pixel viewport exercises browser zoom reflow without CSS
  // `zoom`, which distorts fixed-position dialog units in headless engines.
  await page.setViewportSize({
    width: Math.max(320, Math.floor(viewport.width / 2)),
    height: Math.max(480, Math.floor(viewport.height / 2))
  });
}

export async function denyClipboard(page: Page) {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText() {
          throw new DOMException(
            'Clipboard permission denied',
            'NotAllowedError'
          );
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
        (message.type() === 'error' &&
          !text.startsWith('Failed to load resource:')) ||
        (message.type() === 'warning' &&
          svelteWarningMarkers.some((marker) => text.includes(marker)))
      ) {
        runtimeFailures.push(`Console ${message.type()}: ${text}`);
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
