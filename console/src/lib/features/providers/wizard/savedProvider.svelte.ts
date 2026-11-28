import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
import type { Provider } from '#lib/domain/providers/api.js';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import { getProviderCapabilityOptions, listProviderModelPage } from '../models';
import { probeProvider, type ProviderProbe } from '../providers';

export type SavedProvider = ReturnType<typeof useSavedProvider>;

export function useSavedProvider() {
  const queryClient = useQueryClient();
  let id = $state('');
  let probe = $state.raw<ProviderProbe | null>(null);
  const pagination = $state(emptyCursorHistory());
  const modelsQuery = (providerId: string, cursor: string | undefined) => ({
    queryKey: providerKeys.models(providerId, cursor),
    queryFn: ({ signal }: { signal: AbortSignal }) =>
      listProviderModelPage(providerId, cursor, signal)
  });
  const models = createQuery(() => ({
    ...modelsQuery(id, pagination.cursor),
    enabled: Boolean(id)
  }));
  const capabilityOptions = createQuery(() => {
    const kind = models.data?.provider.configuration.kind;
    return {
      queryKey: providerKeys.capabilityOptions(kind ?? ''),
      queryFn: kind
        ? ({ signal }: { signal: AbortSignal }) =>
            getProviderCapabilityOptions(kind, signal)
        : skipToken
    };
  });
  onDestroy(() => {
    void queryClient.cancelQueries({ queryKey: providerKeys.modelsOf(id) });
  });
  return {
    models,
    capabilityOptions,
    pagination,
    get id() {
      return id;
    },
    get provider() {
      return models.data?.provider ?? null;
    },
    get probe() {
      return probe;
    },
    adopt(providerId: string) {
      id = providerId;
    },
    forgetProbe() {
      probe = null;
    },
    firstPage() {
      resetCursor(pagination);
    },
    refetch() {
      return queryClient.fetchQuery({
        ...modelsQuery(id, pagination.cursor),
        staleTime: 0
      });
    },
    async test(provider: Provider, isCurrent: () => boolean): Promise<void> {
      const { result } = await probeProvider(provider);
      if (!isCurrent()) return;
      probe = result;
      if (!result.succeeded) throw new Error(result.detail);
    }
  };
}
