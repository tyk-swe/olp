import { QueryClient } from '@tanstack/svelte-query';
import { expect, it } from 'vitest';
import { pluginKeys } from './pluginKeys';

it('keeps inventories separate and invalidates every affected view after installation or permission changes', async () => {
  const client = new QueryClient();
  const entries = [
    { key: pluginKeys.list(), value: 'installed' },
    { key: pluginKeys.unconfined(), value: 'native-plugins' },
    { key: pluginKeys.mcpExecutables(), value: 'mcp-executables' },
    { key: pluginKeys.index(), value: 'index' }
  ];
  for (const entry of entries) client.setQueryData(entry.key, entry.value);
  client.setQueryData(['unrelated'], 'retained');
  for (const entry of entries)
    expect(client.getQueryData(entry.key)).toBe(entry.value);
  await client.invalidateQueries({ queryKey: pluginKeys.mcpExecutables() });
  expect(client.getQueryState(pluginKeys.mcpExecutables())?.isInvalidated).toBe(
    true
  );
  expect(client.getQueryState(pluginKeys.list())?.isInvalidated).toBe(false);
  await client.invalidateQueries({ queryKey: pluginKeys.root });
  for (const entry of entries)
    expect(client.getQueryState(entry.key)?.isInvalidated).toBe(true);
  expect(client.getQueryState(['unrelated'])?.isInvalidated).toBe(false);
  client.clear();
});
