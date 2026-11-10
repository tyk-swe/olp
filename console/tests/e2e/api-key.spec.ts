import { expect, route, test } from './journey';

test('a new API key serves the published route and its usage is reported', async ({
  page,
  playwright,
  baseURL,
  vendor
}) => {
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
  await page.getByRole('link', { name: `Usage for ${name}` }).click();
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
});
