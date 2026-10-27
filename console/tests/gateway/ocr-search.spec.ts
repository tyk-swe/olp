import AxeBuilder from '@axe-core/playwright';
import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test, type APIRequestContext, type Page } from '../playwright';
import { takeSecret, waitForRoutePublication } from '../journeys/fixtures';

const upstream = {
  origin: 'http://127.0.0.1:4190',
  model: 'fixture-model',
  credential: 'ocr-search-fixture-secret'
};
const ocrRoute = 'e2e-mistral-ocr';
const azureOcrRoute = 'e2e-azure-ocr';
const nativeSearchRoute = 'e2e-perplexity-search';
const mappedSearchRoute = 'e2e-search-tavily';
const keyName = 'OCR search route key';

async function resetUpstream(request: APIRequestContext) {
  for (let attempt = 0; attempt < 20; attempt++) {
    try {
      const reset = await request.post(`${upstream.origin}/__test__/reset`);
      if (reset.status() === 204) return;
    } catch {
      /* mock still starting */
    }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error('mock upstream did not reset');
}

async function upstreamRequests(request: APIRequestContext) {
  const observed = await request.get(`${upstream.origin}/__test__/requests`);
  return (await observed.json()) as {
    calls: {
      path: string;
      raw: string;
      body: unknown;
      headers: Record<string, string>;
    }[];
    unexpected: string[];
  };
}

// addProvider runs the provider wizard for one openai_compatible vendor
// preset: the preset binds the reviewed operation profile.
async function addProvider(
  page: Page,
  vendor: string,
  endpoint: string,
  operation: string,
  declared = false
) {
  await page.goto('/providers/new');
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
  // The compatible-provider preset binds the reviewed vendor, profile and
  // endpoint; the Vendor control alone leaves the profile unset.
  await page.locator('#compatible-provider').selectOption(vendor.toLowerCase());
  await page.getByLabel('Provider name').fill(`${vendor} upstream`);
  await page
    .getByLabel('Authentication', { exact: true })
    .selectOption('api_key');
  await page
    .getByRole('textbox', { name: 'Endpoint', exact: true })
    .fill(endpoint);
  await page
    .getByLabel(declared ? 'Probe model' : 'Seed model (optional)')
    .fill(upstream.model);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(upstream.credential);
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/api/v1/providers'
  );
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const providerId = ((await (await created).json()) as { id: string }).id;
  await expect(
    page.getByRole('heading', { name: 'Discover upstream models' })
  ).toBeVisible();
  if (!declared) {
    // Vendors without a model-list API declare the probe model directly.
    await page
      .getByRole('button', { name: 'Discover upstream models' })
      .click();
  }
  await expect(
    page.getByRole('heading', { name: 'Review model capabilities' })
  ).toBeVisible();
  await expect(
    page.getByText(upstream.model, { exact: true }).first()
  ).toBeVisible();
  // The declared model arrives capability-empty; discovered models arrive
  // with generation tuples an operation profile cannot certify.
  const removes = page.getByRole('button', { name: /Remove capability/ });
  while ((await removes.count()) > 1) await removes.last().click();
  if (!(await page.getByLabel('Operation 1').isVisible()))
    await page.getByRole('button', { name: 'Add capability' }).click();
  await page.getByLabel('Operation 1').selectOption(operation);
  await page.getByLabel('Client surface 1').selectOption('openai');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText('1 of 1 reviewed tuples passed server certification.')
  ).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();
  return providerId;
}

// addOCRProvider provisions an OCR provider through the management API:
// the provider wizard offers no profile choice for presets that carry no
// default profile, such as mistral's generation preset.
async function addOCRProvider(
  page: Page,
  name: string,
  endpoint: string,
  profile: string,
  vendor: string
) {
  const created = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      body: {
        name,
        configuration: {
          kind: 'openai_compatible',
          auth_mode: 'api_key',
          endpoint,
          profile_id: profile,
          profile_revision: '1',
          options: { vendor_id: vendor }
        },
        credential: upstream.credential,
        model: upstream.model
      },
      idempotency: `create-${vendor}-${Date.now()}`
    }
  );
  expect(created.status, created.source).toBe(201);
  const providerPath = `/api/v1/providers/${created.body.id}`;
  const detail = await manage<{ items: { id: string }[] }>(
    page,
    'GET',
    `${providerPath}/models`
  );
  const modelPath = `${providerPath}/models/${detail.body.items[0]!.id}`;
  const review = await manage(page, 'PATCH', modelPath, {
    match: providerPath,
    body: {
      enabled: true,
      capabilities: [{ operation: 'ocr', surface: 'openai', mode: 'unary' }]
    }
  });
  expect(review.status).toBe(200);
  const certify = await manage(page, 'POST', `${modelPath}/certify`, {
    match: providerPath,
    idempotency: `certify-${Date.now()}`
  });
  expect(certify.status, certify.source).toBe(200);
  const draft = await manage(page, 'POST', `${providerPath}/probe`, {
    match: providerPath,
    idempotency: `probe-${Date.now()}`
  });
  expect(draft.status, draft.source).toBe(200);
  const activated = await manage(page, 'POST', `${providerPath}/activate`, {
    match: providerPath,
    idempotency: `activate-${Date.now()}`
  });
  expect(activated.status, activated.source).toBe(200);
}

// addRoute publishes one route at the given fidelity targeting the provider.
async function addRoute(
  page: Page,
  slug: string,
  fidelity: string,
  operationLabel: string,
  fromLink = true
) {
  if (fromLink) {
    await page.getByRole('link', { name: 'Build default route' }).click();
  } else {
    await page.goto('/routes/new');
  }
  await expect(
    page.getByRole('heading', { name: 'Build a route draft.' })
  ).toBeVisible();
  await page.getByLabel('Public model slug').fill(slug);
  await page
    .getByRole('group', { name: 'Supported operations' })
    .getByRole('checkbox', { name: 'Text generation' })
    .uncheck();
  await page
    .getByRole('group', { name: 'Supported operations' })
    .getByRole('checkbox', { name: operationLabel })
    .check();
  await page.getByRole('button', { name: 'Add target' }).click();
  await page.getByLabel('Fidelity mode').selectOption(fidelity);
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
}

// runNativeOperation drives the playground's public native client.
async function runNative(
  page: Page,
  route: string,
  operation: string,
  template: string,
  secret: string
) {
  await page.goto('/playground');
  await page.getByRole('radio', { name: /Advanced/ }).check();
  await page.locator('#playground-operation').selectOption(operation);
  await page.locator('#playground-template').selectOption(template);
  await page.getByLabel('Route slug').fill(route);
  await page.locator('#native-operation-key').fill(secret);
  await page.getByRole('button', { name: 'Run billable operation' }).click();
}

test('a browser user publishes OCR and search routes and drives them through the playground', async ({
  page,
  request
}, info) => {
  test.setTimeout(240_000);
  await signIn(page);
  await resetUpstream(request);

  await addOCRProvider(
    page,
    'Mistral upstream',
    `${upstream.origin}/mistral/v1`,
    'mistral-ocr',
    'mistral'
  );
  await addRoute(page, ocrRoute, 'strict', 'OCR', false);

  await addOCRProvider(
    page,
    'Azure upstream',
    `${upstream.origin}/azure`,
    'azure-document-intelligence-ocr',
    'azure-document-intelligence'
  );
  await addRoute(page, azureOcrRoute, 'transformed', 'OCR', false);

  await addProvider(
    page,
    'Perplexity',
    `${upstream.origin}/perplexity`,
    'search',
    true
  );
  await addRoute(page, nativeSearchRoute, 'strict', 'Web search');

  await addProvider(
    page,
    'Tavily',
    `${upstream.origin}/tavily`,
    'search',
    true
  );
  await addRoute(page, mappedSearchRoute, 'transformed', 'Web search');

  // One key allows all four routes.
  await page.goto('/api-keys/new');
  await expect(
    page.getByRole('heading', { name: 'Create a proxy key.' })
  ).toBeVisible();
  await page.getByLabel('Key name').fill(keyName);
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  for (const slug of [
    ocrRoute,
    azureOcrRoute,
    nativeSearchRoute,
    mappedSearchRoute
  ]) {
    await page
      .getByRole('group', { name: 'Allowed route slugs' })
      .getByRole('checkbox', { name: slug })
      .check();
  }
  await page.getByRole('button', { name: /Create and show key/ }).click();
  const secretDialog = page.getByRole('dialog', {
    name: 'Copy this secret now.'
  });
  await expect(secretDialog).toBeVisible();
  const secret = await takeSecret(secretDialog);
  expect(secret).toMatch(/^olp_/);
  await waitForRoutePublication(page, secret, ocrRoute);
  await secretDialog
    .getByRole('button', { name: /close|done/i })
    .click()
    .catch(() => page.keyboard.press('Escape'));

  await resetUpstream(request);

  // OCR on the strict route: the serving model is bound to the provider
  // model and the result renders the page markdown.
  const result = page.locator('pre[data-testid="native-operation-result"]');
  await runNative(page, ocrRoute, 'ocr', 'mistral-ocr', secret);
  await expect(result).toContainText('Fixture OCR text');
  await page.screenshot({
    path: info.outputPath('playground-ocr.png'),
    fullPage: true
  });

  // OCR on the transformed Azure route polls inside one request and
  // projects into the frozen page shape.
  await runNative(page, azureOcrRoute, 'ocr', 'mistral-ocr', secret);
  await expect(result).toContainText('Azure fixture OCR text');

  // Native Perplexity search keeps the upstream identity.
  await runNative(
    page,
    nativeSearchRoute,
    'search',
    'perplexity-search',
    secret
  );
  await expect(result).toContainText('ppx_fx_1');
  await expect(result).toContainText('Fixture result');

  // The transformed route projects the Tavily answer into the Perplexity
  // shape with a gateway-minted search_ id.
  await runNative(
    page,
    mappedSearchRoute,
    'search',
    'perplexity-search',
    secret
  );
  await expect(result).toContainText(/search_[0-9a-f]{32}/);
  await expect(result).toContainText('Fixture result');
  await page.screenshot({
    path: info.outputPath('playground-search.png'),
    fullPage: true
  });

  const observed = await upstreamRequests(request);
  expect(observed.unexpected).toEqual([]);
  const byPath = new Map(observed.calls.map((call) => [call.path, call]));
  const ocr = byPath.get('/mistral/v1/ocr');
  expect(ocr?.headers.authorization).toBe(`Bearer ${upstream.credential}`);
  expect((ocr?.body as { model?: string }).model).toBe(upstream.model);
  const tavily = byPath.get('/tavily/search');
  expect(tavily?.headers.authorization).toBe(`Bearer ${upstream.credential}`);
  const tavilyBody = tavily?.body as Record<string, unknown>;
  expect(tavilyBody).toEqual({
    query: 'What is an API gateway?',
    max_results: 3
  });
  const perplexity = byPath.get('/perplexity/search');
  expect(perplexity?.headers.authorization).toBe(
    `Bearer ${upstream.credential}`
  );
  expect((perplexity?.body as { model?: unknown }).model).toBeUndefined();
  const azureSubmit =
    '/azure/documentintelligence/documentModels/fixture-model:analyze';
  const azure = byPath.get(azureSubmit);
  expect(azure?.headers['ocp-apim-subscription-key']).toBe(upstream.credential);
  expect(azure?.headers.authorization).toBeUndefined();
  expect(
    (azure?.body as { base64Source?: unknown }).base64Source
  ).toBeDefined();
  const azurePoll = byPath.get(
    '/azure/documentintelligence/documentModels/fixture-model/analyzeResults/fixture-result'
  );
  expect(azurePoll).toBeDefined();
  expect(
    observed.calls.filter((call) => call.path === azureSubmit)
  ).toHaveLength(1);
  // Only the provider credential ever reached the upstream.
  expect(JSON.stringify(observed.calls)).not.toContain(secret);

  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
