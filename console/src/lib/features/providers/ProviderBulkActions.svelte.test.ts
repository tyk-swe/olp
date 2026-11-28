import { expect, it } from 'vitest';
import { routeQueries } from '#lib/domain/routes/queries.js';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { clickButton, findButton, mountWithState } from '#test/mount.js';
import ProviderBulkActions from './ProviderBulkActions.svelte';
import type { ProviderSummary } from './providers';

const connection: ProviderSummary = {
  id: 'connection-id',
  name: 'Primary connection',
  kind: 'openai',
  state: 'active',
  etag: 'connection-version',
  project_id: null,
  project_name: null,
  capability_count: 1,
  certified_capability_count: 1,
  model_count: 1,
  enabled_model_count: 1,
  connector_ready: true,
  pending_activation: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

it('holds a bulk action until the routes that use its connections are read again', async () => {
  const published = Promise.withResolvers<Response>();
  serve(
    (request) => {
      route(request, 'GET', '/api/v1/routes');
      return published.promise;
    },
    ['read']
  );
  const { target } = mountWithState(
    ProviderBulkActions,
    { selected: [connection], onChanged: () => {} },
    {
      configureClient: (client) => {
        client.setDefaultOptions({ queries: { staleTime: Infinity } });
        client.setQueryData(routeQueries.all().queryKey, []);
      }
    }
  );
  const apply = () =>
    findButton(target, 'Apply disable to reviewed connections');

  clickButton(target, 'Disable selected');

  expect(target.textContent).toContain('Checking affected routes');
  expect(apply()).toBeUndefined();

  published.resolve(
    json({
      items: [
        {
          slug: 'support-chat',
          latest_revision: { targets: [{ provider_id: connection.id }] }
        }
      ],
      next_cursor: null
    })
  );

  await eventually(() => expect(apply()).toBeDefined());
  expect(target.textContent).toContain(
    'Published routes referencing these connections: support-chat.'
  );
});
