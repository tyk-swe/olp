import type { QueryClient } from '@tanstack/svelte-query';
import { overviewKeys } from '#lib/features/overview/overviewKeys.js';
import { errorMessage } from '#lib/management/http.js';
import { providerKeys } from './providerKeys';

export type ProviderChange =
  { kind: 'connection' | 'details' } | { kind: 'activation'; id: string };

export type ProviderRefresh =
  { kind: 'refreshed' } | { kind: 'refresh-failed'; message: string };

export type ProviderChangeReceipt = {
  readonly change: ProviderChange;
  readonly notice: string;
  readonly snapshot?: { readonly previousEtag: string; readonly etag: string };
};

export async function refreshProviderReceipt<T>(
  client: QueryClient,
  receipt: ProviderChangeReceipt,
  read: () => Promise<T>,
  isCurrent: () => boolean
): Promise<
  | { kind: 'refreshed'; value: T }
  | { kind: 'refresh-failed'; message: string }
  | { kind: 'stale' }
> {
  try {
    const value = await read();
    if (!isCurrent()) return { kind: 'stale' };
    const result = await refreshProviderChange(client, receipt.change);
    if (!isCurrent()) return { kind: 'stale' };
    return result.kind === 'refreshed' ? { kind: 'refreshed', value } : result;
  } catch (error) {
    return isCurrent()
      ? { kind: 'refresh-failed', message: errorMessage(error) }
      : { kind: 'stale' };
  }
}

export async function refreshProviderChange(
  client: QueryClient,
  change: ProviderChange
): Promise<ProviderRefresh> {
  const queries =
    change.kind === 'activation'
      ? [
          providerKeys.summaries,
          providerKeys.modelCatalog,
          providerKeys.credentials(change.id),
          providerKeys.networkCredentials(change.id),
          providerKeys.revisionsOf(change.id),
          overviewKeys.root
        ]
      : change.kind === 'details'
        ? [providerKeys.modelCatalog, overviewKeys.root]
        : [providerKeys.summaries, providerKeys.modelCatalog];
  try {
    await Promise.all(
      queries.map((queryKey) =>
        client.invalidateQueries({ queryKey }, { throwOnError: true })
      )
    );
    return { kind: 'refreshed' };
  } catch (error) {
    return { kind: 'refresh-failed', message: errorMessage(error) };
  }
}
