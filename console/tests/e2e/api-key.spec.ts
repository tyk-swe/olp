import { expect, route, test } from './journey';

test('a new API key serves the published route and its usage is reported', async ({
  page,
  playwright,
  baseURL,
  vendor
}, testInfo) => {
  const name = 'browser-key';
  const expiryA = '2099-01-01T12:00';
  const expiryB = '2099-02-01T12:00';
  await page.goto('/keys/new');
  await page.getByRole('textbox', { name: 'Key name' }).fill(name);
  await page.getByLabel('Expires at (optional)', { exact: true }).fill(expiryA);
  await page.getByRole('button', { name: 'Create and show key' }).click();
  const issued = page.getByRole('dialog', { name: 'Copy this secret now.' });
  const secret = (await issued.locator('code').first().innerText()).trim();

  const client = await playwright.request.newContext({ baseURL });
  await expect(async () => {
    const answer = await client.post('/v1/chat/completions', {
      headers: { authorization: `Bearer ${secret}` },
      data: { model: route, messages: [{ role: 'user', content: 'hello' }] }
    });
    expect(answer.status()).toBe(200);
    expect((await answer.json()).choices[0].message.content).toBe(
      vendor.answer
    );
  }).toPass();
  await client.dispose();

  await issued.getByRole('button', { name: 'I have saved the key' }).click();
  const usageLink = page.getByRole('link', { name: `Usage for ${name}` });
  const keyID = new URL(
    (await usageLink.getAttribute('href'))!,
    baseURL
  ).searchParams.get('api_key_id')!;
  await usageLink.click();
  const metric = (label: string) =>
    page
      .getByRole('region', { name: 'Usage summary' })
      .getByRole('article')
      .filter({ has: page.getByText(label, { exact: true }) })
      .locator('strong');
  await expect(async () => {
    await page.getByRole('button', { name: 'Refresh' }).click();
    await expect(metric('Input / output tokens')).toHaveText(
      `${vendor.inputTokens} / ${vendor.outputTokens}`,
      { timeout: 1000 }
    );
  }).toPass();
  await expect(metric('Requests')).toHaveText('1');

  await page.goto(`/keys/${keyID}`);
  await page.getByRole('textbox', { name: 'Key name' }).fill(`${name}-saved`);
  const confirmations: string[] = [];
  page.on('dialog', async (dialog) => {
    confirmations.push(dialog.message());
    await dialog.dismiss();
  });
  const saved = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/v1/api-keys/${keyID}`) &&
      response.request().method() === 'PATCH'
  );
  await page
    .getByRole('button', { name: 'Save and publish', exact: true })
    .click();
  expect((await saved).status()).toBe(200);
  try {
    await expect(page).toHaveURL(/\/keys$/);
    expect(confirmations).toEqual([]);
  } finally {
    await page.screenshot({ path: testInfo.outputPath('saved-key.png') });
  }
  await page.goto(`/keys/${keyID}`);
  await page
    .getByRole('textbox', { name: 'Key name' })
    .fill(`${name}-submitted`);
  await page.getByLabel('Expires at (optional)', { exact: true }).fill(expiryB);
  let releaseSave!: () => void;
  const release = new Promise<void>((resolve) => {
    releaseSave = resolve;
  });
  await page.route(`**/api/v1/api-keys/${keyID}`, async (request) => {
    if (request.request().method() === 'PATCH') {
      const response = await request.fetch();
      await release;
      await request.fulfill({ response });
    } else {
      await request.continue();
    }
  });
  const pending = page.waitForRequest(
    (request) =>
      request.url().endsWith(`/api/v1/api-keys/${keyID}`) &&
      request.method() === 'PATCH'
  );
  const committed = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/v1/api-keys/${keyID}`) &&
      response.request().method() === 'PATCH'
  );
  await page
    .getByRole('button', { name: 'Save and publish', exact: true })
    .click();
  await pending;
  await page
    .getByRole('textbox', { name: 'Key name' })
    .fill(`${name}-newer-edit`);
  await page.getByLabel('Expires at (optional)', { exact: true }).fill(expiryA);
  releaseSave();
  expect((await committed).status()).toBe(200);
  await expect(
    page.getByRole('button', { name: 'Save and publish', exact: true })
  ).toBeEnabled();
  await expect(page.getByRole('textbox', { name: 'Key name' })).toHaveValue(
    `${name}-newer-edit`
  );
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(confirmations).toHaveLength(1);
  await expect(page).toHaveURL(new RegExp(`/keys/${keyID}$`));
  await page.unroute(`**/api/v1/api-keys/${keyID}`);
  const savedNewer = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/v1/api-keys/${keyID}`) &&
      response.request().method() === 'PATCH'
  );
  await page
    .getByRole('button', { name: 'Save and publish', exact: true })
    .click();
  expect((await savedNewer).status()).toBe(200);
  const acknowledged = await page.request.get(`/api/v1/api-keys/${keyID}`);
  expect(acknowledged.status()).toBe(200);
  expect((await acknowledged.json()).expires_at).toBe(`${expiryA}:00Z`);
  await expect(page).toHaveURL(/\/keys$/);
  expect(confirmations).toHaveLength(1);

  await page.goto(`/keys/${keyID}`);
  await page
    .getByRole('textbox', { name: 'Key name' })
    .fill(`${name}-refresh-failed`);
  await page.route(`**/api/v1/api-keys/${keyID}`, async (request) => {
    if (request.request().method() === 'GET') {
      await request.fulfill({
        status: 503,
        contentType: 'application/json',
        body: '{"error":{"code":"unavailable","message":"Try again"}}'
      });
    } else {
      await request.continue();
    }
  });
  await page
    .getByRole('button', { name: 'Save and publish', exact: true })
    .click();
  await expect(page.getByRole('alert')).toContainText(
    'API key saved, but refreshing failed.'
  );
  await expect(page).toHaveURL(new RegExp(`/keys/${keyID}$`));
  await page.unroute(`**/api/v1/api-keys/${keyID}`);
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page).toHaveURL(/\/keys$/);
  expect(confirmations).toHaveLength(1);

  await page.goto(`/keys/${keyID}`);
  await page.getByRole('textbox', { name: 'Key name' }).fill(`${name}-unsaved`);
  await page.route(`**/api/v1/api-keys/${keyID}`, async (request) => {
    if (request.request().method() === 'PATCH') {
      await request.fulfill({
        status: 503,
        contentType: 'application/json',
        body: '{"error":{"code":"unavailable","message":"Try again"}}'
      });
    } else {
      await request.continue();
    }
  });
  const failed = page.waitForResponse(
    (response) =>
      response.url().endsWith(`/api/v1/api-keys/${keyID}`) &&
      response.request().method() === 'PATCH'
  );
  await page
    .getByRole('button', { name: 'Save and publish', exact: true })
    .click();
  expect((await failed).status()).toBe(503);
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(confirmations).toHaveLength(2);
  await expect(page).toHaveURL(new RegExp(`/keys/${keyID}$`));
});
