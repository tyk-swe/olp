import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test, type Page, type TestInfo } from '../playwright';

// PROVIDER_PAGE_SIZE is 50, so fifty-three drafts make Next meaningful and
// leave one searchable outlier.
const providerCount = 52;
const searchable = 'Responsiveness needle';
// The interception holds every list response this long so the update window
// is observable end to end.
const LIST_DELAY_MS = 800;
// Keystrokes arrive faster than the 250ms search debounce.
const KEYSTROKE_MS = 60;

/// Creates draft providers through the signed-in page so the list has real
/// rows; drafts never contact the upstream.
async function provisionProviders(page: Page): Promise<void> {
  const names = [searchable];
  for (let index = 0; index < providerCount; index += 1)
    names.push(`Responsiveness filler ${String(index).padStart(2, '0')}`);
  for (const name of names) {
    const response = await manage(page, 'POST', '/api/v1/providers', {
      idempotency: crypto.randomUUID(),
      body: {
        name,
        configuration: {
          kind: 'openai_compatible',
          auth_mode: 'api_key',
          endpoint: 'http://127.0.0.1:4187/v1'
        },
        credential: 'responsiveness-secret'
      }
    });
    expect(response.status).toBe(201);
  }
}

type ListQuery = { search: string; cursor: string };

/// Records every providers-list request; `started` includes fetches that are
/// later aborted while `finished` only counts completed responses.
function providerGets(page: Page): {
  started: ListQuery[];
  finished: ListQuery[];
} {
  const started: ListQuery[] = [];
  const finished: ListQuery[] = [];
  const record = (url: string, sink: ListQuery[]) => {
    const parsed = new URL(url);
    if (parsed.pathname !== '/api/v1/providers') return;
    sink.push({
      search: parsed.searchParams.get('search') ?? '',
      cursor: parsed.searchParams.get('cursor') ?? ''
    });
  };
  page.on('request', (request) => {
    if (request.method() === 'GET') record(request.url(), started);
  });
  page.on('requestfinished', (request) => {
    if (request.method() === 'GET') record(request.url(), finished);
  });
  return { started, finished };
}

/// Delays every list response while letting it reach the real backend.
async function delayListResponses(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname === '/api/v1/providers',
    async (intercept) => {
      if (intercept.request().method() !== 'GET') return intercept.continue();
      const response = await intercept.fetch();
      await new Promise((resolve) => setTimeout(resolve, LIST_DELAY_MS));
      await intercept.fulfill({ response });
    }
  );
}

const providersRegion = (page: Page) =>
  page.getByRole('region', { name: 'Providers' });
const updating = (page: Page) => page.getByText('Updating…', { exact: true });
const nextButton = (page: Page) =>
  page.getByRole('button', { name: 'Next', exact: true });
const previousButton = (page: Page) =>
  page.getByRole('button', { name: 'Previous', exact: true });
const searchBox = (page: Page) => page.getByLabel('Search connections');
const providerRows = (page: Page) =>
  page.getByRole('row').filter({ hasText: 'Responsiveness' });

test('provider list stays responsive under delayed responses', async ({
  page
}, testInfo: TestInfo) => {
  await signIn(page);
  await provisionProviders(page);
  const requests = providerGets(page);
  await delayListResponses(page);

  await page.goto('/providers');
  await expect(providersRegion(page)).toBeVisible();
  await expect(providerRows(page)).toHaveCount(50);
  await expect(nextButton(page)).toBeEnabled();

  // Paging forward under the delay: the previous rows stay on screen, the
  // region announces it is busy, and pagination cannot run ahead of the data.
  await nextButton(page).click();
  await expect(updating(page)).toBeVisible();
  await expect(providersRegion(page)).toHaveAttribute('aria-busy', 'true');
  await expect(nextButton(page)).toBeDisabled();
  await expect(providerRows(page)).toHaveCount(50);
  await testInfo.attach('providers-updating-page.png', {
    body: await page.screenshot(),
    contentType: 'image/png'
  });
  await expect(updating(page)).toHaveCount(0);
  await expect(providersRegion(page)).toHaveAttribute('aria-busy', 'false');
  await expect(previousButton(page)).toBeEnabled();
  expect(requests.finished.at(-1)?.cursor).not.toBe('');

  // A typing burst applies once the pause elapses: the six keystrokes produce
  // a single completed request carrying the final value.
  const finishedBefore = requests.finished.length;
  const startedBefore = requests.started.length;
  const needleFinished = page.waitForResponse(
    (response) =>
      response.request().method() === 'GET' &&
      new URL(response.url()).pathname === '/api/v1/providers' &&
      new URL(response.url()).searchParams.get('search') === 'needle'
  );
  await searchBox(page).pressSequentially('needle', { delay: KEYSTROKE_MS });
  // While the replacement request is in flight the input stays usable, the
  // previous rows stay rendered, and the status announces the update.
  await expect(searchBox(page)).toBeEnabled();
  await expect(updating(page)).toBeVisible();
  await expect(providerRows(page)).toHaveCount(3);
  await testInfo.attach('providers-updating-search.png', {
    body: await page.screenshot(),
    contentType: 'image/png'
  });
  await needleFinished;
  // `requestfinished` trails the response event by a tick.
  await expect
    .poll(() =>
      requests.finished
        .slice(finishedBefore)
        .some((query) => query.search === 'needle')
    )
    .toBe(true);
  const searchFinished = requests.finished.slice(finishedBefore);
  expect(searchFinished).toEqual([{ search: 'needle', cursor: '' }]);
  // Aborted duplicates may start, but no per-keystroke fan-out is allowed.
  expect(requests.started.slice(startedBefore).length).toBeLessThanOrEqual(2);
  await expect(updating(page)).toHaveCount(0);
  await expect(providerRows(page)).toHaveCount(1);

  // Detail navigation and back keeps the applied search and its results.
  await page.getByRole('link', { name: searchable }).click();
  await expect(page).toHaveURL(/\/providers\/[0-9a-f-]{36}$/);
  await page.goBack();
  await expect(searchBox(page)).toHaveValue('needle');
  await expect(providerRows(page)).toHaveCount(1);
});
