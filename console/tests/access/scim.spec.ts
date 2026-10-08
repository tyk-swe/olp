import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';

test('edits SCIM group grants accessibly without directory credentials or member changes', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const headers = { 'Cache-Control': 'no-store' };
  const project = {
    id: '018f1410-787a-7000-8000-000000000001',
    name: 'Engineering',
    etag: '018f1410-787a-7000-8000-000000000002'
  };
  const group = {
    id: '018f1410-787a-7000-8000-000000000003',
    display_name: 'Builders',
    external_id: 'directory-builders',
    member_count: 12,
    etag: '018f1410-787a-7000-8000-000000000004',
    mapping: {
      role: 'developer',
      accessScope: 'assigned',
      projects: [{ value: project.id, role: 'viewer' }]
    }
  };
  await page.route('**/api/v1/projects?**', (route) =>
    route.fulfill({ headers, json: { items: [project], next_cursor: null } })
  );
  await page.route('**/api/v1/scim/groups*', (route) =>
    route.fulfill({ headers, json: { items: [group], next_cursor: null } })
  );
  await page.route(
    `**/api/v1/scim/groups/${group.id}/mapping`,
    async (route) => {
      expect(route.request().headers()['if-match']).toBe(`"${group.etag}"`);
      expect(route.request().headers()['x-csrf-token']).toBeTruthy();
      const input = route.request().postDataJSON();
      expect(Object.keys(input)).toEqual(['mapping']);
      expect(input.mapping.projects).toEqual([
        { value: project.id, role: 'manager' }
      ]);
      group.mapping = input.mapping;
      group.etag = '018f1410-787a-7000-8000-000000000005';
      await route.fulfill({ headers, json: group });
    }
  );
  await page.goto('/scim-provisioning');
  await page.getByRole('button', { name: 'Edit Builders' }).click();
  await page
    .getByLabel('Project role', { exact: true })
    .selectOption('manager');
  await page.getByRole('button', { name: 'Save group access' }).click();
  await expect(page.getByRole('status')).toContainText('Group access saved.');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(async () => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
    await new Promise<void>((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
    );
  });
  await page.screenshot({
    path: info.outputPath('scim-provisioning.png'),
    fullPage: true
  });
});
