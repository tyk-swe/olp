import { expect, it } from 'vitest';
import type { Organization } from '#lib/domain/organizations/api.js';
import { organizationQueries } from '#lib/domain/organizations/queries.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { clickButton, mountWithState, setFieldValue } from '#test/mount.js';
import OrganizationsPage from './OrganizationsPage.svelte';

const organization: Organization = {
  id: 'organization-id',
  name: 'Operations',
  etag: 'organization-version',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_by: 'owner-id',
  project_count: 0
};

it('gives other screens the new name straight after a rename', async () => {
  let current = organization;
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'PATCH') {
        const { name } = (await request.json()) as Pick<Organization, 'name'>;
        current = { ...current, name };
        return json(current);
      }
      if (path.endsWith('/budget'))
        return json({
          etag: 'budget-version',
          policy: null,
          usage: {
            daily: { accrued: '0' },
            monthly: { accrued: '0' },
            unpriced_attempts: 0
          }
        });
      return json({
        items: path === '/api/v1/organizations' ? [current] : [],
        next_cursor: null
      });
    },
    ['read', 'manage_projects', 'manage_organization']
  );
  const { target, client } = mountWithState(
    OrganizationsPage,
    {},
    {
      attach: true,
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { staleTime: Infinity } })
    }
  );
  const otherScreen = async () =>
    (await client.fetchQuery(organizationQueries.all())).map(
      (item) => item.name
    );
  expect(await otherScreen()).toEqual(['Operations']);
  const rename = await eventually(() => {
    const input = target.querySelector<HTMLInputElement>(
      'input[placeholder="Operations"]'
    );
    expect(input).not.toBeNull();
    return input!;
  });

  setFieldValue(rename, 'Platform');
  clickButton(target, 'Save name');

  await eventually(() =>
    expect(target.querySelector('option:checked')?.textContent?.trim()).toBe(
      'Platform'
    )
  );
  expect(await otherScreen()).toEqual(['Platform']);
});
