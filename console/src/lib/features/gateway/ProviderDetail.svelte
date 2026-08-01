<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ProviderConfigurationSection from './ProviderConfigurationSection.svelte';
  import ProviderCredentialsSection from './ProviderCredentialsSection.svelte';
  import ProviderModelsSection from './ProviderModelsSection.svelte';
  import ProviderRevisionsSection from './ProviderRevisionsSection.svelte';
  import {
    invalidateProviderModelConsumers,
    invalidateProviderSummaries
  } from './providerCache';
  import { isEtagMismatch } from '$lib/api/http';
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
  import {
    activateProvider,
    certifyProviderModel,
    declareProviderModels,
    diffProviderRevisions,
    discoverProviderModels,
    getProvider,
    getProviderCapabilityOptions,
    listProviderCredentials,
    listProviderKinds,
    listProviderModelPage,
    listProviderRevisionPage,
    probeProvider,
    revokeProviderCredential,
    rotateProviderCredential,
    restoreProviderRevision,
    setProviderModel,
    updateProvider,
    type CapabilityCertification,
    type CapabilityDeclaration,
    type Provider,
    type ProviderCredential,
    type ProviderProbe,
    type ProviderRevisionDiff
  } from '$lib/api/management/providers';
  import {
    buildUpdateProviderInput,
    certificationPrerequisiteReady,
    parseManualModelNames,
    providerEditValues,
    type ProviderEditValues
  } from './providerEditor';

  let { providerId }: { providerId: string } = $props();

  type CoordinatedModelPage = {
    page: Awaited<ReturnType<typeof listProviderModelPage>>;
    provider: Provider;
  };

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
  const capabilityOptions = createQuery(() => ({
    queryKey: ['provider-capability-options', provider.data?.kind ?? ''],
    queryFn: ({ signal }) =>
      getProviderCapabilityOptions(provider.data!.kind, signal),
    enabled: Boolean(provider.data)
  }));
  let detailModelCursor = $state<string | undefined>();
  let detailModelHistory = $state<Array<string | undefined>>([]);
  const detailModels = createQuery(() => ({
    queryKey: detailModelPageKey(provider.data, detailModelCursor),
    queryFn: ({ signal }) =>
      fetchDetailModelPage(provider.data!, detailModelCursor, signal),
    enabled: Boolean(provider.data),
    placeholderData: (previous: CoordinatedModelPage | undefined) => previous
  }));
  let retainedModelPage = $state<CoordinatedModelPage>();
  const visibleModelPage = $derived(detailModels.data ?? retainedModelPage);
  const credentials = createQuery(() => ({
    queryKey: ['provider-credentials', providerId],
    queryFn: ({ signal }) => listProviderCredentials(providerId, signal),
    enabled: Boolean(providerId)
  }));
  let revisionCursor = $state<string | undefined>();
  let revisionHistory = $state<Array<string | undefined>>([]);
  const revisions = createQuery(() => ({
    queryKey: ['provider-revisions', providerId, revisionCursor ?? 'first'],
    queryFn: ({ signal }) =>
      listProviderRevisionPage(providerId, revisionCursor, signal),
    enabled: Boolean(providerId)
  }));

  let probe = $state<ProviderProbe | null>(null);
  let manualModelNames = $state('');
  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
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
  let credentialValue = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>(
    {}
  );
  let revisionFrom = $state('');
  let revisionTo = $state('');
  let revisionDiff = $state<ProviderRevisionDiff | null>(null);
  let capabilityReloadVersion = $state(0);
  const concurrentNotice = $derived(conflictNotice(sync));

  $effect(() => {
    const items = revisions.data?.items ?? [];
    if (items.length && !revisionTo) revisionTo = items[0].id;
    if (items.length > 1 && !revisionFrom) revisionFrom = items[1].id;
  });

  $effect(() => {
    const value = provider.data;
    if (!value || !providerSpec) return;
    const next = reconcile(sync, value.etag);
    if (next.state !== sync) sync = next.state;
    if (!next.hydrate) return;
    editValues = providerEditValues(value, providerSpec);
  });

  $effect(() => {
    if (detailModels.data) retainedModelPage = detailModels.data;
  });

  onDestroy(() => {
    credentialValue = '';
  });

  function message(error: unknown) {
    return error instanceof Error
      ? error.message
      : 'The control API could not complete the request.';
  }

  async function run(
    label: string,
    action: () => Promise<void>
  ): Promise<boolean> {
    busy = label;
    errorMessage = '';
    notice = '';
    try {
      await action();
      return true;
    } catch (error) {
      if (isEtagMismatch(error)) sync = markConflict(sync);
      else errorMessage = message(error);
      return false;
    } finally {
      busy = '';
    }
  }

  function touch() {
    sync = markDirty(sync);
  }

  function acceptProvider(updated: Provider) {
    sync = acceptRemote(sync, updated.etag);
    queryClient.setQueryData(['provider', updated.id], updated);
  }

  async function refetchProvider() {
    const result = await provider.refetch();
    if (result.error) throw result.error;
    if (!result.data) throw new Error('The provider reload returned no data.');
    sync = acceptRemote(sync, result.data.etag);
  }

  async function reload() {
    if (busy) return;
    busy = 'reload';
    errorMessage = '';
    notice = '';
    const beforeReload = sync;
    sync = beginReload(sync);
    try {
      const reloadedProvider = await getProvider(providerId);
      const coordinated = await fetchDetailModelPage(
        reloadedProvider,
        detailModelCursor
      );
      cacheDetailModelPage(coordinated, detailModelCursor);
      const next = reconcile(sync, reloadedProvider.etag);
      sync = next.state;
      if (next.hydrate && providerSpec) {
        editValues = providerEditValues(reloadedProvider, providerSpec);
      }
      queryClient.setQueryData(
        ['provider', reloadedProvider.id],
        reloadedProvider
      );
      capabilityReloadVersion += 1;
    } catch (error) {
      // A failed reload is not an edit conflict: restore the previous state so
      // the armed reload flag cannot make a later background refetch discard
      // dirty edits, and report the transport problem instead.
      sync = beforeReload;
      errorMessage = message(error);
    } finally {
      busy = '';
    }
  }

  function clearCertificationResults() {
    certificationResults = {};
  }

  function detailModelPageKey(
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

  async function fetchDetailModelPage(
    providerSnapshot: Provider,
    cursor: string | undefined,
    signal?: AbortSignal
  ): Promise<CoordinatedModelPage> {
    return {
      page: await listProviderModelPage(providerSnapshot.id, cursor, signal),
      provider: providerSnapshot
    };
  }

  function cacheDetailModelPage(
    coordinated: CoordinatedModelPage,
    cursor: string | undefined
  ) {
    queryClient.setQueryData(
      detailModelPageKey(coordinated.provider, cursor),
      coordinated
    );
  }

  async function installProviderWithModels(
    updated: Provider,
    resetToFirstPage: boolean
  ) {
    const cursor = resetToFirstPage ? undefined : detailModelCursor;
    const coordinated = await fetchDetailModelPage(updated, cursor);
    cacheDetailModelPage(coordinated, cursor);
    if (resetToFirstPage) {
      detailModelCursor = undefined;
      detailModelHistory = [];
    }
    acceptProvider(updated);
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: ['provider-model-page', updated.id],
        refetchType: 'none',
        predicate: (query) => query.queryKey[3] !== updated.etag
      }),
      invalidateProviderModelConsumers(queryClient)
    ]);
  }

  function nextRevisionPage() {
    const next = revisions.data?.nextCursor;
    if (!next) return;
    revisionHistory = [...revisionHistory, revisionCursor];
    revisionCursor = next;
    revisionFrom = revisionTo = '';
    revisionDiff = null;
  }

  function previousRevisionPage() {
    revisionCursor = revisionHistory.at(-1);
    revisionHistory = revisionHistory.slice(0, -1);
    revisionFrom = revisionTo = '';
    revisionDiff = null;
  }

  function nextDetailModelPage() {
    const next = detailModels.data?.page.nextCursor;
    if (!next) return;
    detailModelHistory = [...detailModelHistory, detailModelCursor];
    detailModelCursor = next;
  }

  function previousDetailModelPage() {
    detailModelCursor = detailModelHistory.at(-1);
    detailModelHistory = detailModelHistory.slice(0, -1);
  }

  async function compareProviderRevisions() {
    if (
      !providerId ||
      !revisionFrom ||
      !revisionTo ||
      revisionFrom === revisionTo
    )
      return;
    await run('revision-diff', async () => {
      revisionDiff = await diffProviderRevisions(
        providerId,
        revisionFrom,
        revisionTo
      );
    });
  }

  async function restoreRevision(
    current: Provider,
    revisionId: string,
    revision: number
  ) {
    if (
      !confirm(
        `Restore provider revision ${revision} as a new draft? The current credential remains selected.`
      )
    )
      return;
    await run('revision-restore', async () => {
      const restored = await restoreProviderRevision(current, revisionId);
      clearCertificationResults();
      revisionDiff = null;
      await installProviderWithModels(restored, true);
      await invalidateProviderSummaries(queryClient);
      notice = `Revision ${revision} restored as a new draft. Current credential selection was preserved; test and certify before activation.`;
    });
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
      clearCertificationResults();
      await installProviderWithModels(updated, true);
      await invalidateProviderSummaries(queryClient);
      notice = 'Provider draft settings saved.';
    });
  }

  async function testDetail(current: Provider) {
    await run('detail-probe', async () => {
      probe = await probeProvider(current);
      if (!probe.succeeded) throw new Error(probe.detail);
      const updated = await getProvider(current.id);
      acceptProvider(updated);
      await invalidateProviderSummaries(queryClient);
      notice = `Connection succeeded: ${probe.detail}`;
    });
  }

  async function discoverDetail(current: Provider) {
    await run('detail-discover', async () => {
      const updated = await discoverProviderModels(current);
      clearCertificationResults();
      await installProviderWithModels(updated, true);
      await invalidateProviderSummaries(queryClient);
      notice = `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} reviewed.`;
    });
  }

  async function declareDetailModels(current: Provider) {
    const names = parseManualModelNames(manualModelNames);
    if (!names.length) {
      errorMessage = 'Enter at least one upstream model identifier.';
      return;
    }
    await run('detail-declare', async () => {
      const updated = await declareProviderModels(current, names);
      manualModelNames = '';
      clearCertificationResults();
      await installProviderWithModels(updated, true);
      await invalidateProviderSummaries(queryClient);
      notice = `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`;
    });
  }

  async function reviewDetailModel(
    current: Provider,
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) {
    return run(`model-${modelId}`, async () => {
      const updated = await setProviderModel(
        current,
        modelId,
        enabled,
        capabilities
      );
      clearCertificationResults();
      await installProviderWithModels(updated, false);
      await invalidateProviderSummaries(queryClient);
      notice = 'Capability review saved with declared provenance.';
    });
  }

  async function certifyDetailModel(current: Provider, modelId: string) {
    await run(`certify-${modelId}`, async () => {
      if (!certificationPrerequisiteReady(current)) {
        probe = await probeProvider(current);
        if (!probe.succeeded) throw new Error(probe.detail);
        current = {
          ...current,
          last_probe_at: probe.checked_at,
          last_probe_status: 'succeeded',
          last_probe_detail: probe.detail
        };
      }
      const result = await certifyProviderModel(current, modelId);
      certificationResults = { ...certificationResults, [modelId]: result };
      const updated = await getProvider(current.id);
      await installProviderWithModels(updated, false);
      probe = null;
      await invalidateProviderSummaries(queryClient);
      notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
    });
  }

  async function activateDetail(current: Provider) {
    await run('detail-activate', async () => {
      const generation = await activateProvider(current);
      await Promise.all([refetchProvider(), credentials.refetch()]);
      await Promise.all([
        invalidateProviderSummaries(queryClient),
        invalidateProviderModelConsumers(queryClient),
        queryClient.invalidateQueries({
          queryKey: ['provider-revisions', current.id]
        })
      ]);
      notice = `Activated in runtime generation ${generation}.`;
    });
  }

  async function rotateCredential(current: Provider, event: SubmitEvent) {
    event.preventDefault();
    if (!credentialValue) return;
    await run('rotate-credential', async () => {
      await rotateProviderCredential(current, credentialValue);
      credentialValue = '';
      const [updated] = await Promise.all([
        getProvider(current.id),
        credentials.refetch()
      ]);
      clearCertificationResults();
      await installProviderWithModels(updated, true);
      await invalidateProviderSummaries(queryClient);
      notice =
        'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.';
    });
  }

  async function revokeCredential(
    current: Provider,
    credential: ProviderCredential
  ) {
    if (!confirm(`Revoke credential version ${credential.version}?`)) return;
    await run(`revoke-${credential.id}`, async () => {
      await revokeProviderCredential(current, credential.id);
      await Promise.all([refetchProvider(), credentials.refetch()]);
      clearCertificationResults();
      await invalidateProviderSummaries(queryClient);
      notice = `Credential version ${credential.version} revoked.`;
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
{#if providerKinds.isError}<div class="inline-problem" role="alert">
    Provider capabilities could not be loaded. Configuration editing is
    unavailable until a retry succeeds. <button
      class="button button-secondary"
      type="button"
      onclick={() => providerKinds.refetch()}>Retry</button
    >
  </div>{/if}
{#if provider.isError && provider.data}
  <div class="inline-problem" role="alert">
    {message(provider.error)} The last loaded provider remains available below.
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
    {message(provider.error)}
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
      onTouch={touch}
      onSave={() => saveProvider(current)}
      onTest={() => testDetail(current)}
      onActivate={() => activateDetail(current)}
    />
    <ProviderCredentialsSection
      {current}
      {providerSpec}
      credentials={credentials.data ?? []}
      credentialsPending={credentials.isPending}
      bind:credentialValue
      {busy}
      onRotate={(event) => rotateCredential(current, event)}
      onRevoke={(credential) => revokeCredential(current, credential)}
    />
  </div>
  <ProviderModelsSection
    {current}
    {visibleModelPage}
    modelPending={detailModels.isPending}
    modelError={detailModels.isError}
    modelErrorMessage={message(detailModels.error)}
    capabilityOptions={capabilityOptions.data?.capabilities ?? []}
    capabilityOptionsPending={capabilityOptions.isPending}
    capabilityOptionsError={capabilityOptions.isError}
    bind:manualModelNames
    {certificationResults}
    reloadVersion={capabilityReloadVersion}
    {busy}
    page={detailModelHistory.length + 1}
    hasPrevious={detailModelHistory.length > 0}
    onRetry={() => detailModels.refetch()}
    onDiscover={() => discoverDetail(current)}
    onDeclare={() => declareDetailModels(current)}
    onReview={(provider, model, enabled, capabilities) =>
      reviewDetailModel(provider, model.id, enabled, capabilities)}
    onCertify={certifyDetailModel}
    onPrevious={previousDetailModelPage}
    onNext={nextDetailModelPage}
  />
  <ProviderRevisionsSection
    {current}
    revisions={revisions.data?.items ?? []}
    nextCursor={revisions.data?.nextCursor}
    pending={revisions.isPending}
    error={revisions.isError}
    errorMessage={message(revisions.error)}
    bind:revisionFrom
    bind:revisionTo
    {revisionDiff}
    {busy}
    page={revisionHistory.length + 1}
    hasPrevious={revisionHistory.length > 0}
    onRetry={() => revisions.refetch()}
    onCompare={compareProviderRevisions}
    onRestore={(revisionId, revision) =>
      restoreRevision(current, revisionId, revision)}
    onPrevious={previousRevisionPage}
    onNext={nextRevisionPage}
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
