import type { QueryClient } from '@tanstack/svelte-query';
import { queryKeys } from '$lib/api/queryKeys';
import {
  listProviderModelPage,
  type ProviderModelPage
} from '$lib/api/management/providerModels';
import { type Provider } from '$lib/api/management/providers';
import { ApiProblem } from '$lib/api/http';

export type CoordinatedModelPage = {
  page: ProviderModelPage;
  provider: Provider;
};

export type RunProviderAction = (
  label: string,
  action: () => Promise<void>
) => Promise<boolean>;

export function providerModelPageKey(
  provider: Provider,
  cursor: string | undefined
) {
  return [
    'provider-model-page',
    provider.id,
    cursor ?? 'first',
    provider.etag
  ] as const;
}

export async function fetchCoordinatedModelPage(
  provider: Provider,
  cursor: string | undefined,
  signal?: AbortSignal
): Promise<CoordinatedModelPage> {
  const page = await listProviderModelPage(provider.id, cursor, signal);
  if (page.providerEtag !== provider.etag) {
    throw new ApiProblem({
      type: 'https://openllmproxy.dev/problems/etag_mismatch',
      title:
        'The provider changed while loading its model page. Reload to continue.',
      status: 412
    });
  }
  return { page, provider };
}

/**
 * Installs a provider snapshot only after its matching model page is cached.
 * This keeps model mutations pinned to the provider ETag they were rendered
 * from while background consumers transition to the new snapshot.
 *
 * The optional callback runs after the matching page is cached but before the
 * provider ETag becomes visible to reactive consumers. Provider-wide
 * mutations use it to reset pagination atomically.
 */
export async function installProviderWithModels(
  queryClient: QueryClient,
  provider: Provider,
  cursor: string | undefined,
  acceptProvider: (provider: Provider) => void,
  beforeAcceptProvider?: () => void
) {
  const coordinated = await fetchCoordinatedModelPage(provider, cursor);
  queryClient.setQueryData(providerModelPageKey(provider, cursor), coordinated);
  beforeAcceptProvider?.();
  acceptProvider(provider);
  await Promise.all([
    queryClient.invalidateQueries({
      queryKey: queryKeys.providers.modelsOf(provider.id),
      refetchType: 'none'
    }),
    queryClient.invalidateQueries({
      queryKey: queryKeys.providers.modelCatalog
    })
  ]);
}
