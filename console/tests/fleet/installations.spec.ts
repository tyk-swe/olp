import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

const first = 'http://localhost:4197';
const second = 'http://127.0.0.1:4198';
const password = 'a long independent fleet test password';

async function setup(
  page: Page,
  origin: string,
  instance: string,
  email: string
) {
  const directory = process.env.OLP_CONSOLE_E2E_FLEET_SECRET_DIR;
  if (!directory)
    throw new Error(
      'OLP_CONSOLE_E2E_FLEET_SECRET_DIR is required; run make integration.'
    );
  await page.goto(origin);
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill(`${instance} owner`);
  await page.getByLabel('Work email').fill(email);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page
    .getByLabel('Setup token')
    .fill(
      readFileSync(`${directory}/${instance}/bootstrap.token`, 'utf8').trim()
    );
  await page.getByRole('button', { name: 'Create owner account' }).click();
  await expect(
    page.getByRole('heading', { name: 'Bring your first model route online.' })
  ).toBeVisible();
}

async function bookmark(page: Page, name: string, origin: string) {
  await page.getByLabel(/^Choose installation:/).click();
  await page.getByLabel('Bookmark name', { exact: true }).fill(name);
  await page.getByLabel('Installation origin', { exact: true }).fill(origin);
  await page.getByRole('button', { name: 'Add bookmark', exact: true }).click();
}

test('independent installations retain their own sessions, keys, data and saved views', async ({
  page
}, info) => {
  const directory = process.env.OLP_CONSOLE_E2E_FLEET_SECRET_DIR;
  if (!directory)
    throw new Error(
      'Select this integration only with its two disposable installations.'
    );
  await setup(page, first, 'first', 'first-owner@example.com');
  const source = await page.evaluate(async () => {
    const session = await fetch('/api/v1/sessions/current').then((response) =>
      response.json()
    );
    const response = await fetch('/api/v1/projects', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token,
        'Idempotency-Key': crypto.randomUUID()
      },
      body: JSON.stringify({ name: 'First installation project' })
    });
    if (response.status !== 201)
      throw new Error('Could not create the first installation project.');
    const project = await response.json();
    return { sessionId: session.session_id, projectId: project.id };
  });
  await page.goto(`${first}/usage`);
  await page.getByText('Saved views', { exact: true }).click();
  await page
    .getByLabel('View name', { exact: true })
    .fill('First installation view');
  await page.getByRole('button', { name: 'Save current filters' }).click();
  await bookmark(page, 'Second installation', second);
  await page.setViewportSize({ width: 375, height: 812 });
  const originField = await page
    .getByLabel('Installation origin', { exact: true })
    .boundingBox();
  expect(originField).not.toBeNull();
  expect(originField!.x).toBeGreaterThanOrEqual(0);
  expect(originField!.x + originField!.width).toBeLessThanOrEqual(375);
  expect(
    await page
      .getByText('Switching opens that installation with its own session.', {
        exact: false
      })
      .evaluate((element) => element.scrollWidth <= element.clientWidth)
  ).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.setViewportSize({ width: 1280, height: 720 });
  await page
    .getByLabel('Switch installation', { exact: true })
    .selectOption(second);
  await expect(page).toHaveURL(`${second}/setup`);
  expect(
    await page.evaluate(
      async () => (await fetch('/api/v1/sessions/current')).status
    )
  ).toBe(401);
  expect(
    await page.evaluate(() =>
      window.localStorage.getItem('olp.installations.v1')
    )
  ).toBeNull();
  await setup(page, second, 'second', 'second-owner@example.com');
  const destination = await page.evaluate(
    async (projectId) => ({
      session: await fetch('/api/v1/sessions/current').then((response) =>
        response.json()
      ),
      projectStatus: (await fetch(`/api/v1/projects/${projectId}`)).status
    }),
    source.projectId
  );
  expect(destination.session.user.email).toBe('second-owner@example.com');
  expect(destination.session.session_id === source.sessionId).toBe(false);
  expect(destination.projectStatus).toBe(404);
  for (const file of ['auth.key', 'master.json', 'bootstrap.token']) {
    expect(
      readFileSync(`${directory}/first/${file}`).equals(
        readFileSync(`${directory}/second/${file}`)
      )
    ).toBe(false);
  }
  await page.goto(`${second}/usage`);
  await page.getByText('Saved views', { exact: true }).click();
  await expect(
    page.getByRole('option', { name: 'First installation view' })
  ).toHaveCount(0);
  await bookmark(page, 'First installation', first);
  await page
    .getByLabel('Switch installation', { exact: true })
    .selectOption(first);
  await expect(page).toHaveURL(`${first}/overview`);
  expect(
    await page.evaluate(
      async () =>
        (
          await fetch('/api/v1/sessions/current').then((response) =>
            response.json()
          )
        ).user.email
    )
  ).toBe('first-owner@example.com');
  expect(
    await page.evaluate(
      async (id) => (await fetch(`/api/v1/projects/${id}`)).status,
      source.projectId
    )
  ).toBe(200);
  await page.goto(`${first}/usage`);
  await page.getByText('Saved views', { exact: true }).click();
  await expect(
    page.getByRole('option', { name: 'First installation view' })
  ).toHaveCount(1);
  await page.getByLabel(/^Choose installation:/).click();
  await page.screenshot({
    path: info.outputPath('independent-installations.png'),
    fullPage: true
  });
  await page
    .getByLabel('Switch installation', { exact: true })
    .selectOption(second);
  await expect(page).toHaveURL(`${second}/overview`);
  expect(
    await page.evaluate(
      async () =>
        (
          await fetch('/api/v1/sessions/current').then((response) =>
            response.json()
          )
        ).user.email
    )
  ).toBe('second-owner@example.com');
});
