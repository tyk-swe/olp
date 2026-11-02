import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, vi } from 'vitest';
import { agentKeys } from './agentKeys';

it('refreshes shared publication data after a publication update', async () => {
  const client = new QueryClient();
  const fetchAgents = vi
    .fn()
    .mockResolvedValueOnce('old')
    .mockResolvedValueOnce('new');
  const options = {
    queryKey: agentKeys.publication('agents'),
    queryFn: fetchAgents,
    staleTime: Infinity
  };

  expect(await client.fetchQuery(options)).toBe('old');
  expect(client.getQueryData(agentKeys.agents())).toBe('old');
  await client.invalidateQueries({ queryKey: agentKeys.publications });
  expect(await client.fetchQuery(options)).toBe('new');
  expect(client.getQueryData(agentKeys.agents())).toBe('new');
});

it('isolates execution history by project, key and cursor without invalidating grant or server data', async () => {
  const client = new QueryClient();
  const entries = [
    { key: agentKeys.toolCalls('project-a', 'key-a'), value: 'first-page' },
    { key: agentKeys.toolCalls('project-b', 'key-a'), value: 'other-project' },
    { key: agentKeys.toolCalls('project-a', 'key-b'), value: 'other-key' },
    {
      key: agentKeys.toolCalls('project-a', 'key-a', 'next'),
      value: 'next-page'
    },
    { key: agentKeys.grants(), value: 'grants' },
    { key: agentKeys.servers(), value: 'servers' }
  ];
  for (const entry of entries) client.setQueryData(entry.key, entry.value);
  expect(
    client.getQueryData(agentKeys.toolCalls('project-a', 'key-a', ''))
  ).toBe('first-page');
  for (const entry of entries)
    expect(client.getQueryData(entry.key)).toBe(entry.value);
  await client.invalidateQueries({ queryKey: agentKeys.servers() });
  expect(client.getQueryState(agentKeys.servers())?.isInvalidated).toBe(true);
  expect(client.getQueryState(agentKeys.grants())?.isInvalidated).toBe(false);
  expect(
    client.getQueryState(agentKeys.toolCalls('project-b', 'key-a'))
      ?.isInvalidated
  ).toBe(false);
  client.setQueryData(agentKeys.servers(), 'refreshed-servers');
  await client.invalidateQueries({
    queryKey: agentKeys.toolCalls('project-a', 'key-a'),
    exact: true
  });
  expect(
    client.getQueryState(agentKeys.toolCalls('project-a', 'key-a'))
      ?.isInvalidated
  ).toBe(true);
  for (const entry of entries.slice(1))
    expect(client.getQueryState(entry.key)?.isInvalidated).toBe(false);
  await client.invalidateQueries({ queryKey: agentKeys.grants() });
  expect(client.getQueryState(agentKeys.grants())?.isInvalidated).toBe(true);
  expect(client.getQueryState(agentKeys.servers())?.isInvalidated).toBe(false);
  client.clear();
});
