import { expect, route, test } from './journey';

test('a new API key serves the published route and its usage is reported', async ({
  page,
  playwright,
  baseURL,
  vendor
}, testInfo) => {
  const name = 'browser-key';
  await page.goto('/keys/new');
  await page.getByRole('textbox', { name: 'Key name' }).fill(name);
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
  await page.getByRole('link', { name: 'All API keys', exact: true }).click();
  expect(confirmations).toHaveLength(1);
  await expect(page).toHaveURL(new RegExp(`/keys/${keyID}$`));
});
