import { randomUUID } from 'node:crypto';
import AxeBuilder from '@axe-core/playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import { manage } from '../helpers/management';
import { expect, test } from '../playwright';

test('filters task and delegation views by an authorized project', async ({
  page
}) => {
  await signInGatewayOwner(page);
  const name = `Operator views ${randomUUID()}`;
  const project = await manage(page, 'POST', '/api/v1/projects', {
    body: { name },
    idempotency: randomUUID()
  });
  expect(project.status, project.source).toBe(201);
  const projectID = project.body.id;
  if (typeof projectID !== 'string') throw new Error('Project ID is missing');

  for (const view of [
    {
      path: '/a2a-tasks',
      heading: 'A2A tasks',
      endpoint: '/api/v1/a2a-tasks',
      empty: 'No tasks in this view'
    },
    {
      path: '/delegations',
      heading: 'Delegations',
      endpoint: '/api/v1/agent-delegations',
      empty: 'No delegations in this view'
    }
  ]) {
    await page.goto(view.path);
    await expect(
      page.getByRole('heading', { name: view.heading, exact: true })
    ).toBeVisible();
    await expect(page.getByRole('option', { name })).toBeAttached();
    await page.getByLabel('Project').selectOption(projectID);
    const filtered = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname === view.endpoint &&
        url.searchParams.get('project_id') === projectID
      );
    });
    await page.getByRole('button', { name: 'Filter' }).click();
    expect((await filtered).status()).toBe(200);
    await expect(page.getByText(view.empty)).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  }
});
