import { QueryClient } from '@tanstack/svelte-query';
import { expect, it } from 'vitest';
import { catalogKeys } from '#lib/features/providers/model-catalog/catalogKeys.js';
import { projectKeys } from './projectKeys';

it('marks a project publication stale when the model catalog is invalidated', async () => {
  const client = new QueryClient();
  const publication = projectKeys.publication('project-1');
  client.setQueryData(publication, { enabled: true });
  await client.invalidateQueries({ queryKey: catalogKeys.root });
  expect(client.getQueryState(publication)?.isInvalidated).toBe(true);
  client.clear();
});
