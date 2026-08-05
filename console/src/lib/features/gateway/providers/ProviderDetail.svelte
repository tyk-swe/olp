<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import { isEtagMismatch } from '$lib/api/http';
  import {
    getProvider,
    listProviderKinds,
    updateProvider,
    type Provider
  } from '$lib/api/management/providers';
  import {
    acceptRemote,
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markConflict,
    markDirty,
    markSaved,
    reconcile
  } from '$lib/forms/concurrentEdit';
  import ProviderConfigurationSection from './ProviderConfigurationSection.svelte';
  import ProviderCredentialsSection from './ProviderCredentialsSection.svelte';
  import ProviderModelsSection from './ProviderModelsSection.svelte';
  import ProviderRevisionsSection from './ProviderRevisionsSection.svelte';
  import {
    buildUpdateProviderInput,
    providerEditValues,
    type ProviderEditValues
  } from './providerEditor';
  import {
    installProviderWithModels,
    providerDetailError,
    type RunProviderAction
  } from './providerDetailCoordination';
  import { invalidateProviderSummaries } from './providerCache';

  let { providerId }: { providerId: string } = $props();

  const queryClient = useQueryClient();
  const provider = createQuery(() => ({
    queryKey: ['provider', providerId],
    queryFn: ({ signal }) => getProvider(providerId, signal),
    enabled: Boolean(providerId)
  }));
  const providerKinds = createQuery(() => ({
    queryKey: ['provider-kinds'],
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  const providerSpec = $derived(
    providerKinds.data?.find(
      (candidate) => candidate.kind === provider.data?.kind
    )
  );

  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let reloadVersion = $state(0);
  let editValues = $state<ProviderEditValues>({
    name: '',
    endpoint: '',
    apiVersion: '',
    cloudRegion: '',
    cloudProject: '',
    deployment: '',
    authMode: 'api_key'
  });
  let sync = $state(initialConcurrentEdit());
  const concurrentNotice = $derived(conflictNotice(sync));

  $effect(() => {
    const value = provider.data;
    if (!value || !providerSpec) return;
    const next = reconcile(sync, value.etag);
    if (next.state !== sync) sync = next.state;
    if (next.hydrate) editValues = providerEditValues(value, providerSpec);
  });

  const run: RunProviderAction = async (label, action) => {
    busy = label;
    errorMessage = notice = '';
    try {
      await action();
      return true;
    } catch (error) {
      if (isEtagMismatch(error)) sync = markConflict(sync);
      else errorMessage = providerDetailError(error);
      return false;
    } finally {
      busy = '';
    }
  };

  function touch() {
    sync = markDirty(sync);
  }

  function acceptProvider(updated: Provider) {
    sync = acceptRemote(sync, updated.etag);
    queryClient.setQueryData(['provider', updated.id], updated);
  }

  function reportError(message: string) {
    errorMessage = message;
    notice = '';
  }

  function reportNotice(message: string) {
    errorMessage = '';
    notice = message;
  }

  async function reload() {
    if (busy) return;
    busy = 'reload';
    errorMessage = notice = '';
    const beforeReload = sync;
    sync = beginReload(sync);
    try {
      const reloaded = await getProvider(providerId);
      const next = reconcile(sync, reloaded.etag);
      sync = next.state;
      if (next.hydrate && providerSpec) {
        editValues = providerEditValues(reloaded, providerSpec);
      }
      queryClient.setQueryData(['provider', reloaded.id], reloaded);
      reloadVersion += 1;
    } catch (error) {
      sync = beforeReload;
      errorMessage = providerDetailError(error);
    } finally {
      busy = '';
    }
  }

  async function saveProvider(current: Provider) {
    if (!providerSpec) return;
    await run('save', async () => {
      if (!sync.snapshotEtag)
        throw new Error('Reload the provider before saving.');
      const updated = await updateProvider(
        current.id,
        sync.snapshotEtag,
        buildUpdateProviderInput(editValues, providerSpec)
      );
      sync = markSaved(sync, updated.etag);
      await installProviderWithModels(
        queryClient,
        updated,
        undefined,
        acceptProvider
      );
      await invalidateProviderSummaries(queryClient);
      reportNotice('Provider draft settings saved.');
    });
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway · Provider</p>
    <h1 class="page-title">{provider.data?.name ?? 'Provider detail'}</h1>
    <p class="page-description">
      Test identity, review models and capability evidence, and rotate
      write-only credentials.
    </p>
  </div>
  <a class="button button-secondary" href={resolve('/providers')}
    >All providers</a
  >
</div>

{#if errorMessage}<div class="inline-problem" role="alert">
    {errorMessage}
  </div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}
<ConflictNotice
  notice={concurrentNotice}
  onReload={reload}
  disabled={Boolean(busy)}
/>

{#if providerKinds.isError}
  <div class="inline-problem" role="alert">
    Provider capabilities could not be loaded. Configuration editing is
    unavailable until a retry succeeds. <button
      class="button button-secondary"
      type="button"
      onclick={() => providerKinds.refetch()}>Retry</button
    >
  </div>
{/if}
{#if provider.isError && provider.data}
  <div class="inline-problem" role="alert">
    {providerDetailError(provider.error)} The last loaded provider remains available
    below.
    <button
      class="button button-secondary"
      type="button"
      onclick={() => provider.refetch()}>Retry</button
    >
  </div>
{/if}

{#if provider.isPending && !provider.data}
  <div class="loading-state" role="status">Loading provider…</div>
{:else if !provider.data}
  <div class="inline-problem" role="alert">
    {providerDetailError(provider.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => provider.refetch()}>Retry</button
    >
  </div>
{:else}
  {@const current = provider.data}
  {#if current.pending_activation}<div class="pending-banner" role="status">
      <strong>Revision {current.active_revision} remains live.</strong><span
        >Draft configuration and the draft-selected credential are not serving
        traffic. Test, certify, and activate to replace the runtime revision
        atomically.</span
      >
    </div>{/if}
  <div class="detail-grid">
    <ProviderConfigurationSection
      {current}
      {providerSpec}
      bind:editValues
      {busy}
      {run}
      onTouch={touch}
      onSave={() => saveProvider(current)}
      onAcceptProvider={acceptProvider}
      onNotice={reportNotice}
    />
    <ProviderCredentialsSection
      {current}
      {providerSpec}
      {busy}
      {run}
      onAcceptProvider={acceptProvider}
      onNotice={reportNotice}
    />
  </div>
  <ProviderModelsSection
    {current}
    {busy}
    {run}
    {reloadVersion}
    onAcceptProvider={acceptProvider}
    onError={reportError}
    onNotice={reportNotice}
  />
  <ProviderRevisionsSection
    {current}
    {busy}
    {run}
    onAcceptProvider={acceptProvider}
    onNotice={reportNotice}
  />
{/if}

<style>
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  .pending-banner {
    display: grid;
    gap: 0.2rem;
    margin: 1rem 0;
    padding: 0.9rem 1rem;
    border: 1px solid color-mix(in srgb, var(--warning) 55%, var(--border));
    border-radius: 0.375rem;
    background: var(--warning-soft);
    color: var(--foreground);
  }
  .pending-banner span {
    color: var(--foreground-muted);
    font-size: 0.82rem;
  }
  .detail-grid {
    display: grid;
    grid-template-columns: minmax(0, 1.35fr) minmax(18rem, 0.65fr);
    gap: 1rem;
  }
  @media (max-width: 64rem) {
    .detail-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
