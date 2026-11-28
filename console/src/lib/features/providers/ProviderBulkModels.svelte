<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import { serializeIfMatch } from '#lib/management/http.js';
  import { providerKeys } from '#lib/domain/providers/providerKeys.js';
  import type { Provider } from '#lib/domain/providers/api.js';
  import { listProviderModelPage } from './models';
  import { useBulkModelRun } from './bulkModelRun.svelte.js';
  import BulkModelInputs from './BulkModelInputs.svelte';
  import BulkModelStatus from './BulkModelStatus.svelte';
  let {
    provider,
    canManage,
    onChanged
  }: {
    provider: Provider;
    canManage: boolean;
    onChanged: () => void | Promise<void>;
  } = $props();
  const models = createQuery(() => {
    const id = provider.id;
    const etag = provider.etag;
    return {
      queryKey: providerKeys.bulkModels(id, etag),
      queryFn: async ({ signal }) => {
        let snapshot: Provider | undefined;
        const items = await collectCursorPages(async (cursor) => {
          const page = await listProviderModelPage(id, cursor, signal);
          if (
            snapshot &&
            serializeIfMatch(snapshot.etag) !==
              serializeIfMatch(page.provider.etag)
          ) {
            throw new Error(
              'The connection changed while models were loaded. Refresh before reviewing them.'
            );
          }
          snapshot ??= page.provider;
          return page;
        });
        if (!snapshot)
          throw new Error('The model inventory has no connection version.');
        return { items, provider: snapshot };
      }
    };
  });
  const run = useBulkModelRun({
    provider: () => provider,
    canManage: () => canManage,
    inventory: () => models.data,
    reload: async (isCurrent) => {
      if (!isCurrent()) return;
      await onChanged();
      if (!isCurrent()) return;
      await models.refetch({ throwOnError: true });
    }
  });
</script>

<section class="card bulk" aria-labelledby="bulk-heading">
  <p class="eyebrow">Bulk validation</p>
  <h2 id="bulk-heading">Validate models in bulk</h2>
  <p class="description">
    Select the models to enable. Validation uses bounded upstream requests and
    records a result for each capability.
  </p>
  {#if models.isError}<div class="inline-problem" role="alert">
      Models could not be loaded. <button
        class="button button-secondary"
        type="button"
        onclick={() => models.refetch()}
      >
        Retry
      </button>
    </div>{/if}
  <BulkModelInputs
    items={models.data?.items ?? []}
    bind:operation={run.selection.operation}
    bind:selected={run.selection.models}
    busy={run.busy}
    {canManage}
  />
  <BulkModelStatus
    {canManage}
    busy={run.busy}
    refreshPending={run.report.refreshPending}
    selectedCount={run.selection.models.length}
    failedCount={run.report.failedIds.length}
    status={run.report.status}
    failures={run.report.failures.map((failure) => failure.message)}
    onValidate={run.validate}
    onRetryFailed={run.retryFailed}
    onRetryRefresh={run.retryRefresh}
    onStop={run.stop}
  />
</section>

<style>
  .bulk {
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .description {
    color: var(--foreground-muted);
  }
  .description {
    margin-bottom: 1rem;
  }
</style>
