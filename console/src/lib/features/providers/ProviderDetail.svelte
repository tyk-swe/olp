<script lang="ts">
  import ProviderBulkModels from './ProviderBulkModels.svelte';
  import ProviderCredentialPool from './ProviderCredentialPool.svelte';
  import ProviderOptions from './ProviderOptions.svelte';
  import { focusErrorSummary } from '$lib/forms/focusError';
  import { providerKeys } from '$lib/features/providers/providerKeys';

  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import { resolve } from '$app/paths';
  import {
    createQuery,
    keepPreviousData,
    useQueryClient
  } from '@tanstack/svelte-query';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    errorMessage as providerDetailError,
    fieldIssues,
    isEtagMismatch,
    type FieldIssue
  } from '$lib/api/http';
  import { updateProvider, type Provider } from '$lib/features/providers/api';
  import {
    listProviderModelPage,
    listProviderKinds,
    type ProviderModelPage
  } from '$lib/features/providers/models';
  import { emptyCursorHistory, resetCursor } from '$lib/lists/pagination';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
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
  import ProviderConfigurationSection from '$lib/features/providers/ProviderConfigurationSection.svelte';
  import ProviderCredentialsSection from '$lib/features/providers/ProviderCredentialsSection.svelte';
  import ProviderModelsSection from '$lib/features/providers/ProviderModelsSection.svelte';
  import ProviderRevisionsSection from '$lib/features/providers/ProviderRevisionsSection.svelte';
  import ProviderValidationIssues from '$lib/features/providers/ProviderValidationIssues.svelte';
  import {
    buildUpdateProviderInput,
    providerEditValues,
    type ProviderEditValues
  } from '$lib/features/providers/providerEditor';
  import type { RunProviderAction } from './providerEditor';

  let { providerId }: { providerId: string } = $props();

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('providers.manage'));
  let modelPageState = $state(emptyCursorHistory());
  const snapshot = createQuery(() => ({
    queryKey: providerKeys.models(providerId, modelPageState.cursor),
    queryFn: ({ signal }) =>
      listProviderModelPage(providerId, modelPageState.cursor, signal),
    enabled: Boolean(providerId),
    placeholderData: keepPreviousData
  }));
  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  const providerSpec = $derived.by(() => {
    const kind = snapshot.data?.provider?.configuration.kind;
    const kinds = providerKinds.data;
    return kinds?.find((candidate) => candidate.kind === kind);
  });

  let busy = $state('');
  let editVersion = 0;
  let errorMessage = $state('');
  let saveRefreshError = $state('');
  let validationIssues = $state<FieldIssue[]>([]);
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
    const value = snapshot.data?.provider;
    if (!value || !providerSpec) return;
    const next = reconcile(sync, value.etag);
    if (next.state !== sync) sync = next.state;
    if (next.hydrate) editValues = providerEditValues(value, providerSpec);
  });

  const run: RunProviderAction = async (label, action) => {
    if (busy) return false;
    busy = label;
    errorMessage = notice = '';
    validationIssues = [];
    try {
      await action();
      return true;
    } catch (error) {
      if (isEtagMismatch(error)) sync = markConflict(sync);
      else {
        errorMessage = providerDetailError(error);
        validationIssues = fieldIssues(error);
      }
      return false;
    } finally {
      busy = '';
    }
  };

  function touch() {
    editVersion += 1;
    sync = markDirty(sync);
  }

  guardUnsavedChanges(() => sync.dirty);

  async function providerChanged() {
    const result = await snapshot.refetch();
    if (result.error) throw result.error;
    if (!result.data) throw new Error('The provider snapshot is unavailable.');
    sync = acceptRemote(sync, result.data.provider.etag);
    await queryClient.invalidateQueries({
      queryKey: providerKeys.modelCatalog
    });
  }

  function reportError(message: string) {
    errorMessage = message;
    validationIssues = [];
    notice = '';
  }

  function reportNotice(message: string) {
    errorMessage = '';
    validationIssues = [];
    notice = message;
  }

  function resetModelPage() {
    resetCursor(modelPageState);
  }

  async function refetchProvider(): Promise<boolean> {
    const result = await snapshot.refetch();
    if (result.error || !result.data) return false;
    sync = acceptRemote(sync, result.data.provider.etag);
    return true;
  }

  async function reload() {
    if (busy) return;
    busy = 'reload';
    errorMessage = notice = '';
    validationIssues = [];
    try {
      const result = await snapshot.refetch();
      if (result.error) throw result.error;
      if (!result.data)
        throw new Error('The provider snapshot is unavailable.');
      sync = beginReload(sync);
      sync = acceptRemote(sync, result.data.provider.etag);
      if (providerSpec)
        editValues = providerEditValues(result.data.provider, providerSpec);
      reloadVersion += 1;
    } catch (error) {
      errorMessage = providerDetailError(error);
    } finally {
      busy = '';
    }
  }

  async function saveProvider(current: Provider) {
    if (!providerSpec || !canManage || busy) return;
    await run('save', async () => {
      if (!sync.snapshotEtag)
        throw new Error('Reload the provider before saving.');
      const submittedVersion = editVersion;
      const updated = await updateProvider(
        current.id,
        sync.snapshotEtag,
        buildUpdateProviderInput(editValues, providerSpec)
      );
      sync = markSaved(updated.etag, editVersion !== submittedVersion);
      queryClient.setQueriesData<ProviderModelPage>(
        { queryKey: providerKeys.modelsOf(current.id) },
        (page) => (page ? { ...page, provider: updated } : page)
      );
      resetModelPage();
      await refreshSavedProvider();
    });
  }

  async function refreshSavedProvider() {
    saveRefreshError = '';
    try {
      await providerChanged();
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
    } catch (error) {
      saveRefreshError = providerDetailError(error);
    }
    reportNotice(
      sync.dirty
        ? 'Draft saved. You have additional unsaved changes.'
        : 'Provider draft settings saved.'
    );
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway · Provider</p>
    <h1 class="page-title">
      {snapshot.data?.provider?.name ?? 'Provider detail'}
    </h1>
    <p class="page-description">
      Test identity, review models and capability evidence, and rotate
      write-only credentials.
    </p>
    {#if snapshot.data?.provider}<p class="provider-meta">
        Created by {snapshot.data?.provider.created_by_email ??
          'a removed account'}
      </p>{/if}
  </div>
  <a class="button button-secondary" href={resolve('/providers')}
    >All providers</a
  >
</div>
{#if saveRefreshError}
  <div class="inline-problem" role="alert">
    <p>Draft saved, but refreshing failed. {saveRefreshError}</p>
    <button
      class="button button-secondary"
      type="button"
      disabled={Boolean(busy)}
      onclick={() => run('refresh', refreshSavedProvider)}>Retry refresh</button
    >
  </div>
{/if}

{#if errorMessage}<div
    class="inline-problem"
    role="alert"
    tabindex="-1"
    use:focusErrorSummary
  >
    {errorMessage}
    <ProviderValidationIssues issues={validationIssues} />
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
{#if snapshot.isError && snapshot.data?.provider && !saveRefreshError}
  <div class="inline-problem" role="alert">
    {providerDetailError(snapshot.error)} The last loaded provider remains available
    below.
    <button
      class="button button-secondary"
      type="button"
      onclick={() => snapshot.refetch()}>Retry</button
    >
  </div>
{/if}

{#if providerKinds.isPending || (snapshot.isPending && !snapshot.data)}
  <div class="loading-state" role="status">Loading provider…</div>
{:else if !snapshot.data?.provider}
  <div class="inline-problem" role="alert">
    {providerDetailError(snapshot.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => snapshot.refetch()}>Retry</button
    >
  </div>
{:else}
  {@const current = snapshot.data?.provider}
  {#if !canManage}<ReadOnlyNote>
      Your role can view this provider but not change, test, or activate it.
    </ReadOnlyNote>{/if}
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
      {canManage}
      {run}
      onTouch={touch}
      onSave={() => saveProvider(current)}
      onProviderChanged={providerChanged}
      onRefetchProvider={refetchProvider}
      onNotice={reportNotice}
    />
    <ProviderCredentialsSection
      {current}
      {providerSpec}
      {busy}
      {canManage}
      {run}
      onProviderChanged={providerChanged}
      onResetModelPage={resetModelPage}
      onNotice={reportNotice}
    />
  </div>
  {#if canManage}<a
      class="button button-secondary"
      href={`${resolve('/providers/new')}?copy=${current.id}`}
      >Duplicate connection</a
    >{/if}
  <ProviderCredentialPool
    provider={current}
    {canManage}
    onChanged={providerChanged}
  />
  <ProviderOptions provider={current} {canManage} onChanged={providerChanged} />
  <ProviderBulkModels
    provider={current}
    {canManage}
    onChanged={providerChanged}
  />
  <ProviderModelsSection
    modelSnapshot={snapshot.data}
    loading={snapshot.isPending}
    error={snapshot.error}
    reloadModels={providerChanged}
    {current}
    {busy}
    {canManage}
    {run}
    {reloadVersion}
    bind:pageState={modelPageState}
    onProviderChanged={providerChanged}
    onError={reportError}
    onConflict={() => {
      sync = markConflict(sync);
    }}
    onNotice={reportNotice}
  />
  <ProviderRevisionsSection
    {current}
    {busy}
    {canManage}
    {run}
    onProviderChanged={providerChanged}
    onResetModelPage={resetModelPage}
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
  .provider-meta {
    margin: 0.35rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
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
