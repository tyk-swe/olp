<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listRoutes } from '$lib/features/routes/api';
  import {
    getProvider,
    activateProvider,
    disableProvider,
    restoreProviderAsDraft,
    type ProviderSummary
  } from './api';
  import { errorMessage } from '$lib/api/http';
  let {
    selected,
    onChanged
  }: { selected: ProviderSummary[]; onChanged: () => void | Promise<void> } =
    $props();
  let preview = $state<'disable' | 'activate' | 'restore' | null>(null);
  let busy = $state(false);
  let outcomes = $state<string[]>([]);
  const routes = createQuery(() => ({
    queryKey: ['provider-bulk-route-preview'],
    enabled: Boolean(preview),
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const affected = $derived(
    routes.data?.filter((route) =>
      route.latest_revision.targets.some((target) =>
        selected.some((provider) => provider.id === target.provider_id)
      )
    ) ?? []
  );
  async function apply() {
    if (!preview || routes.isPending || routes.isError) return;
    busy = true;
    outcomes = [];
    for (const selectedProvider of selected) {
      try {
        const provider = await getProvider(selectedProvider.id);
        if (provider.etag !== selectedProvider.etag)
          throw new Error('Connection changed; reload and review it again.');
        if (preview === 'disable') await disableProvider(provider);
        else if (preview === 'restore') await restoreProviderAsDraft(provider);
        else await activateProvider(provider);
        outcomes.push(
          `${provider.name}: ${preview === 'activate' ? 'activated' : preview === 'disable' ? 'disabled' : 'restored as draft'}.`
        );
      } catch (error) {
        outcomes.push(`${selectedProvider.name}: ${errorMessage(error)}`);
      }
    }
    busy = false;
    preview = null;
    await onChanged();
  }
</script>

{#if selected.length}
  <section class="card bulk-actions" aria-label="Selected connections">
    <strong>{selected.length} connections selected</strong>
    <div class="actions">
      {#each [{ action: 'activate', label: 'Activate selected' }, { action: 'disable', label: 'Disable selected' }, { action: 'restore', label: 'Restore as drafts' }] as item (item.action)}<button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={() => {
            preview = item.action as typeof preview;
            outcomes = [];
          }}>{item.label}</button
        >{/each}
    </div>
    {#if preview}<p>
        Review {selected.map((provider) => provider.name).join(', ')} before applying
        {preview}.
      </p>
      {#if routes.isPending}<p role="status">
          Checking affected routes…
        </p>{:else if routes.isError}<p role="alert">
          Route preview unavailable. <button onclick={() => routes.refetch()}
            >Retry</button
          >
        </p>{:else}<p>
          {affected.length
            ? `Published routes referencing these connections: ${affected.map((route) => route.slug).join(', ')}.`
            : 'No published routes reference these connections.'}
        </p>
        <p>
          {preview === 'disable'
            ? 'Remove referenced targets before disabling a connection. Connections with live media jobs remain protected.'
            : preview === 'activate'
              ? 'Each connection must have valid credentials and certified models. Changes apply to the listed routes.'
              : 'Connections return as drafts; review and validate them before activation.'}
        </p>
        <button class="button button-primary" disabled={busy} onclick={apply}
          >{busy
            ? 'Applying…'
            : `Apply ${preview} to reviewed connections`}</button
        >{/if}
    {/if}
  </section>
{/if}
{#if outcomes.length}<ul role="status">
    {#each outcomes as outcome (outcome)}<li>{outcome}</li>{/each}
  </ul>{/if}

<style>
  .bulk-actions {
    padding: 1.5rem;
    margin: 1rem 0;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin: 0.75rem 0;
  }
  p {
    color: var(--foreground-muted);
  }
</style>
