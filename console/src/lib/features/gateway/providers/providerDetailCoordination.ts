import type { QueryClient } from '@tanstack/svelte-query';
import {
  listProviderModelPage,
  type Provider,
  type ProviderModel
} from '$lib/api/management/providers';
import type { CursorPage } from '$lib/api/management/shared';
import { invalidateProviderModelConsumers } from './providerCache';

export type CoordinatedModelPage = {
  page: CursorPage<ProviderModel>;
  provider: Provider;
};

export type RunProviderAction = (
  label: string,
  action: () => Promise<void>
) => Promise<boolean>;

export function providerDetailError(error: unknown): string {
  return error instanceof Error
    ? error.message
    : 'The control API could not complete the request.';
}

export function providerModelPageKey(
  providerId: string,
  providerSnapshot: Provider | undefined,
  cursor: string | undefined
) {
  return [
    'provider-model-page',
    providerSnapshot?.id ?? providerId,
    cursor ?? 'first',
    providerSnapshot?.etag ?? 'unversioned'
  ] as const;
}

export async function fetchCoordinatedModelPage(
  provider: Provider,
  cursor: string | undefined,
  signal?: AbortSignal
): Promise<CoordinatedModelPage> {
  return {
    page: await listProviderModelPage(provider.id, cursor, signal),
    provider
  };
}

export function cacheCoordinatedModelPage(
  queryClient: QueryClient,
  coordinated: CoordinatedModelPage,
  cursor: string | undefined
) {
  queryClient.setQueryData(
    providerModelPageKey(coordinated.provider.id, coordinated.provider, cursor),
    coordinated
  );
}

/**
 * Installs a provider snapshot only after its matching model page is cached.
 * This keeps model mutations pinned to the provider ETag they were rendered
 * from while background consumers transition to the new snapshot.
 */
export async function installProviderWithModels(
  queryClient: QueryClient,
  provider: Provider,
  cursor: string | undefined,
  acceptProvider: (provider: Provider) => void
) {
  const coordinated = await fetchCoordinatedModelPage(provider, cursor);
  cacheCoordinatedModelPage(queryClient, coordinated, cursor);
  acceptProvider(provider);
  await Promise.all([
    queryClient.invalidateQueries({
      queryKey: ['provider-model-page', provider.id],
      refetchType: 'none',
      predicate: (query) => query.queryKey[3] !== provider.etag
    }),
    invalidateProviderModelConsumers(queryClient)
  ]);
}
