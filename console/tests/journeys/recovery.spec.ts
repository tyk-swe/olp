import { expect, test } from '../playwright';
import {
  vertical,
  signInAsOwner,
  takeSecret,
  waitForRoutePublication
} from './fixtures';

test('restored installation preserves identity, credentials and request history', async ({
  page
}) => {
  test.skip(
    process.env.OLP_CONSOLE_E2E_RESTORED !== 'true',
    'Requires a restored installation.'
  );
  await signInAsOwner(page);
  await page.goto('/providers');
  await expect(
    page.getByRole('link', { name: 'Concurrent provider name', exact: true })
  ).toBeVisible();
  await page.goto('/requests');
  await expect(
    page.getByRole('heading', { name: 'Request Explorer', exact: true })
  ).toBeVisible();
  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('Restored gateway request');
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  await page.getByRole('button', { name: /Create and show key/ }).click();
  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  const secret = await takeSecret(dialog);
  await waitForRoutePublication(page, secret, vertical.route);
  const result = await page.evaluate(
    async ({ secret, model }) => {
      const response = await fetch('/v1/responses', {
        method: 'POST',
        headers: {
          authorization: `Bearer ${secret}`,
          'content-type': 'application/json'
        },
        body: JSON.stringify({
          model,
          input: 'Connection test',
          max_output_tokens: 16
        })
      });
      const body = await response.json();
      return {
        status: response.status,
        output: body.output?.[0]?.content?.[0]?.text
      };
    },
    { secret, model: vertical.route }
  );
  expect(result).toEqual({ status: 200, output: vertical.reply });
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(vertical.route);
  await page.getByLabel('Prompt', { exact: true }).fill('Connection test');
  await page.getByLabel('Max output tokens').fill('16');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.getByText(vertical.reply, { exact: true })).toBeVisible();
});
