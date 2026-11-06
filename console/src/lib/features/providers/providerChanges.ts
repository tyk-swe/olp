import type { QueryClient } from '@tanstack/svelte-query';
import { overviewKeys } from '$lib/features/overview/overviewKeys';
import { errorMessage } from '$lib/management/http';
import { providerKeys } from './providerKeys';

export type ProviderChange =
  { kind: 'connection' | 'details' } | { kind: 'activation'; id: string };

export type ProviderRefresh =
  { kind: 'refreshed' } | { kind: 'refresh-failed'; message: string };

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
