import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test } from '../playwright';

test('creating a project preserves the organization selected while its response is pending', async ({
  page
}) => {
  await signIn(page);
  const organizationA = await manage(page, 'POST', '/api/v1/organizations', {
    idempotency: crypto.randomUUID(),
    body: { name: 'Project creation origin' }
  });
  expect(organizationA.status).toBe(201);
  const organizationB = await manage(page, 'POST', '/api/v1/organizations', {
    idempotency: crypto.randomUUID(),
    body: { name: 'Project creation destination' }
  });
  expect(organizationB.status).toBe(201);
  const idA = organizationA.body.id as string;
  const idB = organizationB.body.id as string;
  const existingProject = await manage(
    page,
    'POST',
    `/api/v1/organizations/${idB}/projects`,
    {
      idempotency: crypto.randomUUID(),
      body: { name: 'Destination project' }
    }
  );
  expect(existingProject.status).toBe(201);

  const releaseCreation = Promise.withResolvers<void>();
  const sawCreation = Promise.withResolvers<void>();
  const releaseRefresh = Promise.withResolvers<void>();
  let holdRefresh = false;
  let createdProjectId = '';
  const mismatchedRequests: string[] = [];
  page.on('request', (request) => {
    if (
      createdProjectId &&
      request
        .url()
        .includes(`/organizations/${idB}/projects/${createdProjectId}`)
    )
      mismatchedRequests.push(request.url());
  });
  const originProjects = `**/api/v1/organizations/${idA}/projects*`;
  const destinationProjects = `**/api/v1/organizations/${idB}/projects*`;
  await page.route(originProjects, async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    const response = await route.fetch();
    expect(response.status()).toBe(201);
    createdProjectId = (await response.json()).id as string;
    sawCreation.resolve();
    await releaseCreation.promise;
    await route.fulfill({ response });
  });
  await page.route(destinationProjects, async (route) => {
    if (holdRefresh) await releaseRefresh.promise;
    await route.continue();
  });
  try {
    await page.goto('/organizations');
    const organization = page.getByRole('combobox', {
      name: 'Organization',
      exact: true
    });
    const project = page.getByRole('combobox', {
      name: 'Organization project',
      exact: true
    });
    await organization.selectOption(idA);
    await page
      .getByLabel('New project name', { exact: true })
      .fill('Origin team');
    await page
      .getByRole('button', { name: 'Create organization project', exact: true })
      .click();
    await sawCreation.promise;
    await expect(organization).toBeEnabled();
    await organization.selectOption(idB);
    await expect(project).toHaveValue(existingProject.body.id as string);

    // Keep the refresh pending so it cannot hide an incorrect cache update.
    holdRefresh = true;
    releaseCreation.resolve();
    await expect(page.getByText('Saved.', { exact: true })).toBeVisible();
    await expect(organization).toHaveValue(idB);
    await expect(project).toHaveValue(existingProject.body.id as string);
    await expect(project.locator('option')).toHaveText(['Destination project']);
    expect(mismatchedRequests).toEqual([]);

    releaseRefresh.resolve();
    await expect(
      page.getByRole('button', {
        name: 'Create organization project',
        exact: true
      })
    ).toBeEnabled();
    await organization.selectOption(idA);
    await expect(project).toHaveValue(createdProjectId);
    await expect(project.locator('option')).toHaveText(['Origin team']);
    expect(mismatchedRequests).toEqual([]);
  } finally {
    releaseCreation.resolve();
    releaseRefresh.resolve();
    await page.unroute(originProjects);
    await page.unroute(destinationProjects);
  }
});
