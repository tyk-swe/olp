import { askPlayground, expect, owner, route, session, test } from './journey';

test('an operator sets up the installation and brings a route online', async ({
  page,
  vendor
}) => {
  await test.step('create the owner account', async () => {
    await page.goto('/');
    await expect(page).toHaveURL(/\/setup$/);
    await page.getByLabel('Display name').fill(owner.name);
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page
      .getByLabel('Setup token')
      .fill(process.env.OLP_E2E_SETUP_TOKEN ?? '');
    await page.getByRole('button', { name: 'Create owner account' }).click();
    await expect(
      page.getByRole('heading', {
        name: 'Bring your first model route online.'
      })
    ).toBeVisible();
  });

  await test.step('connect and activate a provider', async () => {
    await page
      .getByRole('link', { name: 'Connect and activate a provider' })
      .first()
      .click();
    await page
      .getByRole('combobox', { name: 'Vendor' })
      .selectOption({ label: 'OpenAI-compatible' });
    await page
      .getByRole('textbox', { name: 'Provider name' })
      .fill('browser-vendor');
    await page.getByRole('textbox', { name: 'Endpoint' }).fill(vendor.endpoint);
    await page.getByRole('textbox', { name: 'Credential' }).fill(vendor.secret);
    await page
      .getByRole('button', { name: 'Save and test connection' })
      .click();
    await page
      .getByRole('button', { name: 'Discover upstream models' })
      .click();
    await page.getByRole('checkbox', { name: vendor.model }).check();
    await page
      .getByRole('button', { name: 'Validate 1 selected models' })
      .click();
    await expect(page.getByText('Checked 1 models')).toBeVisible();
    await page.getByRole('button', { name: 'Continue to activation' }).click();
    await page.getByRole('button', { name: 'Test completed draft' }).click();
    await page.getByRole('button', { name: 'Activate provider' }).click();
    await expect(page.getByText('Provider activated')).toBeVisible();
  });

  await test.step('publish a route to it', async () => {
    await page.getByRole('link', { name: 'Build default route' }).click();
    await page.getByRole('textbox', { name: 'Public model slug' }).fill(route);
    await page.getByRole('combobox', { name: 'Fidelity mode' }).selectOption({
      label: 'Transformed · translate or redact deliberately'
    });
    await page.getByRole('button', { name: 'Add target' }).click();
    await expect(
      page.getByRole('combobox', { name: 'Provider model' })
    ).toContainText(vendor.model);
    await page.getByRole('button', { name: 'Create draft' }).click();
    await page.getByRole('button', { name: 'Validate draft' }).click();
    await expect(page.getByText('Validation passed')).toBeVisible();
    await page.getByRole('button', { name: 'Activate route' }).click();
    await expect(page.getByText('Route activated')).toBeVisible();
  });

  await test.step('wait until the gateway serves it', async () => {
    await expect(async () => {
      await expect(await askPlayground(page, false)).toContainText(
        vendor.answer,
        { timeout: 1000 }
      );
    }).toPass();
  });

  await page.context().storageState({ path: session });
});
