<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import CapabilityReview from './CapabilityReview.svelte';
  import {
    invalidateProviderModelConsumers,
    invalidateProviderSummaries
  } from './providerCache';
  import { ApiProblem } from '$lib/api/http';
  import {
    activateProvider,
    certifyProviderModel,
    declareProviderModels,
    diffProviderRevisions,
    discoverProviderModels,
    getProvider,
    getProviderCapabilityOptions,
    listProviderCredentials,
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
    activationReady,
    buildUpdateProviderInput,
    capabilitiesCertified,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    parseManualModelNames,
    probeReady,
    providerEditValues,
    providerStatus,
    type ProviderEditValues
  } from './providerEditor';

  let { providerId }: { providerId: string } = $props();

  const queryClient = useQueryClient();
  const provider = createQuery(() => ({
    queryKey: ['provider', providerId],
    queryFn: ({ signal }) => getProvider(providerId, signal),
    enabled: Boolean(providerId)
  }));
  const capabilityOptions = createQuery(() => ({
    queryKey: ['provider-capability-options', provider.data?.kind ?? ''],
    queryFn: ({ signal }) => getProviderCapabilityOptions(provider.data!.kind, signal),
    enabled: Boolean(provider.data)
  }));
  let detailModelCursor = $state<string | undefined>();
  let detailModelHistory = $state<Array<string | undefined>>([]);
  const detailModels = createQuery(() => ({
    queryKey: ['provider-model-page', providerId, detailModelCursor ?? 'first'],
    queryFn: ({ signal }) => listProviderModelPage(providerId, detailModelCursor, signal),
    enabled: Boolean(providerId)
  }));
  const credentials = createQuery(() => ({
    queryKey: ['provider-credentials', providerId],
    queryFn: ({ signal }) => listProviderCredentials(providerId, signal),
    enabled: Boolean(providerId)
  }));
  let revisionCursor = $state<string | undefined>();
  let revisionHistory = $state<Array<string | undefined>>([]);
  const revisions = createQuery(() => ({
    queryKey: ['provider-revisions', providerId, revisionCursor ?? 'first'],
    queryFn: ({ signal }) => listProviderRevisionPage(providerId, revisionCursor, signal),
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
  let detailInitialized = $state('');
  let credentialValue = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>({});
  let revisionFrom = $state('');
  let revisionTo = $state('');
  let revisionDiff = $state<ProviderRevisionDiff | null>(null);

  $effect(() => {
    const items = revisions.data?.items ?? [];
    if (items.length && !revisionTo) revisionTo = items[0].id;
    if (items.length > 1 && !revisionFrom) revisionFrom = items[1].id;
  });

  $effect(() => {
    const value = provider.data;
    if (!value || detailInitialized === value.etag) return;
    detailInitialized = value.etag;
    editValues = providerEditValues(value);
  });

  onDestroy(() => {
    credentialValue = '';
  });

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error
        ? error.message
        : 'The control API could not complete the request.';
  }

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    errorMessage = '';
    notice = '';
    try {
      await action();
    } catch (error) {
      errorMessage = message(error);
    } finally {
      busy = '';
    }
  }

  function clearCertificationResults() {
    certificationResults = {};
  }

  async function resetDetailModels(providerId: string) {
    detailModelCursor = undefined;
    detailModelHistory = [];
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: ['provider-model-page', providerId],
        refetchType: 'none'
      }),
      invalidateProviderModelConsumers(queryClient)
    ]);
    await queryClient.refetchQueries({
      queryKey: ['provider-model-page', providerId, 'first'],
      exact: true,
      type: 'all'
    });
  }

  async function refreshCurrentDetailModelPage(providerId: string) {
    const queryKey = ['provider-model-page', providerId, detailModelCursor ?? 'first'];
    await Promise.all([
      queryClient.invalidateQueries({ queryKey, exact: true, refetchType: 'none' }),
      invalidateProviderModelConsumers(queryClient)
    ]);
    await queryClient.refetchQueries({ queryKey, exact: true, type: 'all' });
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
    const next = detailModels.data?.nextCursor;
    if (!next) return;
    detailModelHistory = [...detailModelHistory, detailModelCursor];
    detailModelCursor = next;
  }

  function previousDetailModelPage() {
    detailModelCursor = detailModelHistory.at(-1);
    detailModelHistory = detailModelHistory.slice(0, -1);
  }

  async function compareProviderRevisions() {
    if (!providerId || !revisionFrom || !revisionTo || revisionFrom === revisionTo) return;
    await run('revision-diff', async () => {
      revisionDiff = await diffProviderRevisions(providerId, revisionFrom, revisionTo);
    });
  }

  async function restoreRevision(current: Provider, revisionId: string, revision: number) {
    if (!confirm(`Restore provider revision ${revision} as a new draft? The current credential remains selected.`)) return;
    await run('revision-restore', async () => {
      const restored = await restoreProviderRevision(current, revisionId);
      queryClient.setQueryData(['provider', providerId], restored);
      clearCertificationResults();
      revisionDiff = null;
      await resetDetailModels(current.id);
      await invalidateProviderSummaries(queryClient);
      notice = `Revision ${revision} restored as a new draft. Current credential selection was preserved; test and certify before activation.`;
    });
  }

  async function saveProvider(current: Provider) {
    await run('save', async () => {
      const updated = await updateProvider(
        current.id,
        current.etag,
        buildUpdateProviderInput(current, editValues)
      );
      queryClient.setQueryData(['provider', current.id], updated);
      clearCertificationResults();
      await resetDetailModels(current.id);
      await invalidateProviderSummaries(queryClient);
      notice = 'Provider draft settings saved.';
    });
  }

  async function testDetail(current: Provider) {
    await run('detail-probe', async () => {
      probe = await probeProvider(current);
      if (!probe.succeeded) throw new Error(probe.detail);
      const updated = await getProvider(current.id);
      queryClient.setQueryData(['provider', current.id], updated);
      await invalidateProviderSummaries(queryClient);
      notice = `Connection succeeded: ${probe.detail}`;
    });
  }

  async function discoverDetail(current: Provider) {
    await run('detail-discover', async () => {
      const updated = await discoverProviderModels(current);
      queryClient.setQueryData(['provider', current.id], updated);
      clearCertificationResults();
      await resetDetailModels(current.id);
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
      queryClient.setQueryData(['provider', current.id], updated);
      clearCertificationResults();
      await resetDetailModels(current.id);
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
    await run(`model-${modelId}`, async () => {
      const updated = await setProviderModel(current, modelId, enabled, capabilities);
      queryClient.setQueryData(['provider', current.id], updated);
      clearCertificationResults();
      await refreshCurrentDetailModelPage(current.id);
      await invalidateProviderSummaries(queryClient);
      notice = 'Capability review saved with declared provenance.';
    });
  }

  async function certifyDetailModel(current: Provider, modelId: string) {
    await run(`certify-${modelId}`, async () => {
      const result = await certifyProviderModel(current, modelId);
      certificationResults = { ...certificationResults, [modelId]: result };
      const updated = await getProvider(current.id);
      queryClient.setQueryData(['provider', current.id], updated);
      await refreshCurrentDetailModelPage(current.id);
      probe = null;
      await invalidateProviderSummaries(queryClient);
      notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
    });
  }

  async function activateDetail(current: Provider) {
    await run('detail-activate', async () => {
      const generation = await activateProvider(current);
      await Promise.all([provider.refetch(), credentials.refetch()]);
      await Promise.all([
        invalidateProviderSummaries(queryClient),
        invalidateProviderModelConsumers(queryClient),
        queryClient.invalidateQueries({ queryKey: ['provider-revisions', current.id] })
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
      await Promise.all([provider.refetch(), credentials.refetch()]);
      clearCertificationResults();
      await resetDetailModels(current.id);
      await invalidateProviderSummaries(queryClient);
      notice = 'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.';
    });
  }

  async function revokeCredential(current: Provider, credential: ProviderCredential) {
    if (!confirm(`Revoke credential version ${credential.version}?`)) return;
    await run(`revoke-${credential.id}`, async () => {
      await revokeProviderCredential(current, credential.id);
      await Promise.all([provider.refetch(), credentials.refetch()]);
      clearCertificationResults();
      await invalidateProviderSummaries(queryClient);
      notice = `Credential version ${credential.version} revoked.`;
    });
  }
</script>

<div class="page-header">
  <div><p class="eyebrow">Gateway · Provider</p><h1 class="page-title">{provider.data?.name ?? 'Provider detail'}</h1><p class="page-description">Test identity, review models and capability evidence, and rotate write-only credentials.</p></div>
  <a class="button button-secondary" href="/providers">All providers</a>
</div>

{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}
{#if provider.isPending}
  <div class="loading-state" role="status">Loading provider…</div>
{:else if provider.isError}
  <div class="inline-problem" role="alert">{message(provider.error)} <button class="button button-secondary" type="button" onclick={() => provider.refetch()}>Retry</button></div>
{:else if provider.data}
  {@const current = provider.data}
  {#if current.pending_activation}<div class="pending-banner" role="status"><strong>Revision {current.active_revision} remains live.</strong><span>Draft configuration and the draft-selected credential are not serving traffic. Test, certify, and activate to replace the runtime revision atomically.</span></div>{/if}
  <div class="detail-grid">
    <section class="card editor" aria-labelledby="configuration-heading">
      <div class="section-heading"><div><p class="eyebrow">Configuration</p><h2 id="configuration-heading">Connector context</h2></div><span class:success={current.active_revision != null && !current.pending_activation} class:warning={current.pending_activation} class="badge">{providerStatus(current)}</span></div>
      <div class="form-grid">
        <div class="form-field"><label for="detail-name">Name</label><input id="detail-name" bind:value={editValues.name} /></div>
        <div class="form-field"><label for="detail-auth">Authentication</label><input id="detail-auth" value={editValues.authMode} disabled /><small>Identity mode is immutable.</small></div>
        {#if hasCustomEndpoint(current.kind)}<div class="form-field full"><label for="detail-endpoint">Endpoint</label><input id="detail-endpoint" bind:value={editValues.endpoint} /></div>{/if}
        {#if hasApiVersion(current.kind)}<div class="form-field"><label for="detail-version">API version</label><input id="detail-version" bind:value={editValues.apiVersion} /></div>{/if}
        {#if hasCloudRegion(current.kind)}<div class="form-field"><label for="detail-region">Cloud region</label><input id="detail-region" bind:value={editValues.cloudRegion} /></div>{/if}
        {#if hasCloudProject(current.kind)}<div class="form-field"><label for="detail-project">Cloud project</label><input id="detail-project" bind:value={editValues.cloudProject} /></div>{/if}
        {#if hasDeployment(current.kind)}<div class="form-field"><label for="detail-deployment">Cloud deployment</label><input id="detail-deployment" bind:value={editValues.deployment} /></div>{/if}
      </div>
      <ol class="activation-checklist compact" aria-label="Provider activation requirements"><li class:complete={capabilitiesCertified(current)}>{capabilitiesCertified(current) ? '✓' : '1'} Capabilities certified</li><li class:complete={probeReady(current)}>{probeReady(current) ? '✓' : '2'} Completed draft tested</li></ol>
      <div class="form-actions"><button class="button button-secondary" type="button" onclick={() => saveProvider(current)} disabled={Boolean(busy)}>Save draft</button><button class="button button-secondary" type="button" onclick={() => testDetail(current)} disabled={Boolean(busy) || current.state !== 'draft' || !capabilitiesCertified(current)}>{busy === 'detail-probe' ? 'Testing completed draft…' : 'Test completed draft'}</button><button class="button button-primary" type="button" onclick={() => activateDetail(current)} disabled={Boolean(busy) || !activationReady(current)}>Activate changes</button></div>
      {#if current.last_probe_at}<p class="audit-note">Last probe {new Date(current.last_probe_at).toLocaleString()}: {current.last_probe_status} — {current.last_probe_detail}</p>{/if}
    </section>
    <section class="card editor" aria-labelledby="credential-heading">
      <p class="eyebrow">Secrets</p><h2 id="credential-heading">Credential versions</h2><p class="muted">The API never returns secret material. Rotation selects a draft version; the runtime credential remains live until activation.</p>
      {#if ['adc', 'default_chain'].includes(current.auth_mode)}<div class="identity-note"><strong>{current.auth_mode === 'adc' ? 'Application Default Credentials' : 'AWS default chain'}</strong><span>Identity is supplied by this deployment; there is no encrypted credential version.</span></div>{:else}<form class="credential-form" onsubmit={(event) => rotateCredential(current, event)}><label class="sr-only" for="rotation-secret">New credential</label><input id="rotation-secret" type="password" autocomplete="new-password" bind:value={credentialValue} placeholder="New credential" /><button class="button button-secondary" type="submit" disabled={!credentialValue || Boolean(busy)}>{busy === 'rotate-credential' ? 'Staging…' : 'Stage rotation'}</button></form>{/if}
      {#if credentials.isPending}<p role="status">Loading versions…</p>{:else}<ul class="credential-list">{#each credentials.data ?? [] as credential (credential.id)}<li><span><strong>Version {credential.version}</strong><small>{new Date(credential.created_at).toLocaleString()}</small></span><span class:success={credential.active} class:warning={credential.draft_selected && !credential.active} class:danger={Boolean(credential.revoked_at)} class="badge">{credential.revoked_at ? 'revoked' : credential.active && credential.draft_selected ? 'runtime active · draft selected' : credential.active ? 'runtime active' : credential.draft_selected ? 'pending activation' : 'retired'}</span>{#if !credential.active && !credential.draft_selected && !credential.revoked_at}<button class="button button-secondary" type="button" onclick={() => revokeCredential(current, credential)} disabled={Boolean(busy)}>Revoke</button>{/if}</li>{/each}</ul>{/if}
    </section>
  </div>
  <section class="card editor models" aria-labelledby="models-heading">
    <div class="section-heading"><div><p class="eyebrow">Discovery</p><h2 id="models-heading">Models and capabilities</h2></div><a class="button button-secondary" href="/models">Inventory view</a></div>
    <div class="discovery-row"><p class="muted">Refresh the inventory from the upstream model-list API. Existing capability certification is reconciled server-side.</p><button class="button button-secondary" type="button" onclick={() => discoverDetail(current)} disabled={Boolean(busy)}>{busy === 'detail-discover' ? 'Discovering…' : 'Run upstream discovery'}</button></div>
    {#if current.kind === 'openai_compatible'}<details class="manual-fallback"><summary>Manual model identifiers</summary><p>Use only if this compatible endpoint has no list API. Models remain disabled until capability review.</p><div class="form-field"><label for="manual-models-detail">Upstream model identifiers</label><textarea id="manual-models-detail" bind:value={manualModelNames} placeholder="model-a&#10;model-b"></textarea></div><button class="button button-secondary" type="button" onclick={() => declareDetailModels(current)} disabled={Boolean(busy)}>{busy === 'detail-declare' ? 'Adding…' : 'Add identifiers for review'}</button></details>{/if}
    {#if current.model_count === 0}<div class="empty-state"><p>No models have been discovered.</p></div>{:else if detailModels.isPending}<div class="loading-state" role="status">Loading models…</div>{:else if detailModels.isError}<div class="inline-problem" role="alert">{message(detailModels.error)} <button class="button button-secondary" type="button" onclick={() => detailModels.refetch()}>Retry</button></div>{:else}<div class="table-shell"><table class="data-table"><thead><tr><th>Model</th><th>Explicit capability review</th></tr></thead><tbody>{#each detailModels.data?.items ?? [] as model (model.id)}<tr><td><strong>{model.display_name}</strong><br /><code>{model.upstream_model}</code></td><td><CapabilityReview {model} options={capabilityOptions.data?.capabilities ?? []} optionsPending={capabilityOptions.isPending} optionsError={capabilityOptions.isError} disabled={Boolean(busy)} onSave={(enabled, capabilities) => reviewDetailModel(current, model.id, enabled, capabilities)} /><div class="certification-action"><button class="button button-secondary" type="button" onclick={() => certifyDetailModel(current, model.id)} disabled={Boolean(busy) || !model.capabilities.length}>{busy === `certify-${model.id}` ? 'Server-certifying…' : 'Server-certify capabilities'}</button>{#if certificationResults[model.id]}{@const result = certificationResults[model.id]}<span class:success={result.status === 'succeeded'} class:warning={result.status !== 'succeeded'}>{result.certified_count}/{result.attempted_count} certified</span><ul class="certification-results">{#each result.results.filter((item) => !item.succeeded) as item (`${item.operation}-${item.surface}-${item.mode}`)}<li><code>{item.operation}/{item.surface}/{item.mode}</code>: {item.detail}</li>{/each}</ul>{/if}</div></td></tr>{/each}</tbody></table></div><CursorPagination page={detailModelHistory.length + 1} hasPrevious={detailModelHistory.length > 0} hasNext={Boolean(detailModels.data?.nextCursor)} onPrevious={previousDetailModelPage} onNext={nextDetailModelPage} label="Provider model pages" />{/if}
    {#if !activationReady(current)}<p class="audit-note">Every native and compatible tuple requires fresh server-owned certification. After the last change or certification, run the completed-draft connection test before activation.</p>{/if}
  </section>
  <section class="card editor revisions" aria-labelledby="provider-revisions-heading">
    <div class="section-heading"><div><p class="eyebrow">Immutable history</p><h2 id="provider-revisions-heading">Provider revisions</h2><p class="muted">Historical secrets and credential IDs are never returned. Restoring copies only non-secret configuration into a new draft and preserves the current credential selection.</p></div></div>
    {#if revisions.isPending}<p role="status">Loading provider revisions…</p>
    {:else if revisions.isError}<div class="inline-problem" role="alert">{message(revisions.error)} <button class="button button-secondary" type="button" onclick={() => revisions.refetch()}>Retry</button></div>
    {:else if revisions.data?.items.length === 0 && revisionHistory.length === 0}<div class="empty-state"><p>No activated revision exists yet.</p></div>
    {:else}
      {#if (revisions.data?.items.length ?? 0) > 1}
        <div class="revision-compare" aria-label="Compare provider revisions">
          <label>From<select bind:value={revisionFrom}>{#each revisions.data?.items ?? [] as item (item.id)}<option value={item.id}>Revision {item.revision}</option>{/each}</select></label>
          <label>To<select bind:value={revisionTo}>{#each revisions.data?.items ?? [] as item (item.id)}<option value={item.id}>Revision {item.revision}</option>{/each}</select></label>
          <button class="button button-secondary" type="button" onclick={compareProviderRevisions} disabled={!revisionFrom || !revisionTo || revisionFrom === revisionTo || Boolean(busy)}>{busy === 'revision-diff' ? 'Comparing…' : 'Compare'}</button>
        </div>
      {/if}
      {#if revisionDiff}
        <div class="revision-diff" role="region" aria-label={`Provider revision ${revisionDiff.from_revision} to ${revisionDiff.to_revision} diff`}>
          <h3>Revision {revisionDiff.from_revision} → {revisionDiff.to_revision}</h3>
          <ul class="diff-flags">
            {#if revisionDiff.name_changed}<li>Name changed</li>{/if}
            {#if revisionDiff.connector_changed}<li>Connector changed</li>{/if}
            {#if revisionDiff.endpoint_changed}<li>Endpoint changed</li>{/if}
            {#if revisionDiff.cloud_context_changed}<li>Cloud context changed</li>{/if}
            {#if revisionDiff.deployment_changed}<li>Deployment changed</li>{/if}
            {#if revisionDiff.api_version_changed}<li>API version changed</li>{/if}
            {#if revisionDiff.credential_changed}<li>Credential version changed (secret remains redacted)</li>{/if}
          </ul>
          <div class="diff-columns"><div><strong>Models added</strong><ul>{#each revisionDiff.models_added as value (value)}<li><code>{value}</code></li>{/each}</ul></div><div><strong>Models changed</strong><ul>{#each revisionDiff.models_changed as value (value)}<li><code>{value}</code></li>{/each}</ul></div><div><strong>Models removed</strong><ul>{#each revisionDiff.models_removed as value (value)}<li><code>{value}</code></li>{/each}</ul></div><div><strong>Capabilities added</strong><ul>{#each revisionDiff.capabilities_added as value (value)}<li><code>{value}</code></li>{/each}</ul></div><div><strong>Capabilities removed</strong><ul>{#each revisionDiff.capabilities_removed as value (value)}<li><code>{value}</code></li>{/each}</ul></div></div>
        </div>
      {/if}
      <div class="revision-list">
        {#each revisions.data?.items ?? [] as item (item.id)}
          <article class="revision-row"><div><strong>Revision {item.revision}</strong><small>Activated {new Date(item.activated_at).toLocaleString()} by <code>{item.activated_by}</code></small><small>{item.model_count} models · credential metadata {item.historical_credential_version == null ? 'workload identity' : `version ${item.historical_credential_version}`}</small></div><button class="button button-secondary" type="button" onclick={() => restoreRevision(current, item.id, item.revision)} disabled={Boolean(busy)}>{busy === 'revision-restore' ? 'Restoring…' : 'Restore as draft'}</button></article>
        {/each}
      </div>
      <CursorPagination page={revisionHistory.length + 1} hasPrevious={revisionHistory.length > 0} hasNext={Boolean(revisions.data?.nextCursor)} onPrevious={previousRevisionPage} onNext={nextRevisionPage} label="Provider revision pages" />
    {/if}
  </section>
{/if}

<style>
  .editor { max-width: 66rem; margin-top: 1.25rem; padding: clamp(1.15rem, 3vw, 1.75rem); }
  h2 { margin: 0 0 .85rem; font-size: 1.15rem; font-weight: 750; letter-spacing: -.025em; }
  .muted, .audit-note, .credential-list small { color: var(--foreground-muted); }
  .form-actions { display: flex; flex-wrap: wrap; gap: .65rem; margin-top: 1.35rem; }
  .success-banner { margin: 1rem 0; padding: .85rem 1rem; border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border)); border-radius: .375rem; background: var(--success-soft); color: var(--success); }
  .pending-banner { display: grid; gap: .2rem; margin: 1rem 0; padding: .9rem 1rem; border: 1px solid color-mix(in srgb, var(--warning) 55%, var(--border)); border-radius: .375rem; background: var(--warning-soft); color: var(--foreground); }
  .pending-banner span { color: var(--foreground-muted); font-size: .82rem; }
  .revisions { max-width: none; }
  .revision-compare { display: flex; flex-wrap: wrap; align-items: end; gap: .65rem; margin: 1rem 0; }
  .revision-compare label { display: grid; gap: .3rem; color: var(--foreground-muted); font-size: .72rem; font-weight: 700; }
  .revision-compare select { min-height: 2.5rem; padding: .5rem .65rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .revision-diff { margin: 1rem 0; padding: 1rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface-subtle); }
  .revision-diff h3 { margin: 0; font-size: 1rem; }
  .diff-flags { display: flex; flex-wrap: wrap; gap: .4rem 1.2rem; padding-left: 1.2rem; }
  .diff-columns { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .75rem; }
  .diff-columns ul { margin: .35rem 0 0; padding-left: 1.1rem; }
  .diff-columns li { overflow-wrap: anywhere; }
  .revision-list { display: grid; gap: .6rem; }
  .revision-row { display: flex; align-items: center; justify-content: space-between; gap: 1rem; padding: .85rem; border: 1px solid var(--border); border-radius: .375rem; }
  .revision-row > div { display: grid; min-width: 0; gap: .2rem; }
  .revision-row small { color: var(--foreground-muted); overflow-wrap: anywhere; }
  .detail-grid { display: grid; grid-template-columns: minmax(0, 1.35fr) minmax(18rem, .65fr); gap: 1rem; }
  .detail-grid .editor { max-width: none; }
  .section-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; }
  .credential-form, .discovery-row { display: flex; align-items: end; gap: .6rem; }
  .certification-action { display: flex; flex-wrap: wrap; align-items: center; gap: .6rem; margin-top: .75rem; padding-top: .75rem; border-top: 1px solid var(--border); color: var(--foreground-muted); font-size: .75rem; }
  .activation-checklist { display: grid; gap: .4rem; margin: 1rem 0 0; padding: 0; list-style: none; color: var(--foreground-muted); font-size: .8rem; }
  .activation-checklist li { min-height: 1.5rem; }
  .activation-checklist li.complete { color: var(--success); font-weight: 700; }
  .activation-checklist.compact { margin-top: 1.1rem; }
  .certification-results { width: 100%; margin: 0; padding-left: 1.25rem; color: var(--danger); }
  .credential-form input { min-width: 0; min-height: 2.5rem; flex: 1; padding: .5rem .7rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .credential-list { margin: 1rem 0 0; padding: 0; list-style: none; }
  .credential-list li { display: flex; min-height: 3.5rem; align-items: center; gap: .6rem; border-top: 1px solid var(--border); }
  .credential-list li > span:first-child { display: grid; margin-right: auto; }
  .models { max-width: none; }
  .manual-fallback { margin-top: 1rem; padding: .75rem; border: 1px solid var(--border); border-radius: .375rem; }
  .manual-fallback summary { min-height: 2.75rem; font-weight: 720; }
  .manual-fallback p { color: var(--foreground-muted); font-size: .78rem; }
  .manual-fallback textarea { min-height: 5rem; }
  .identity-note { display: grid; gap: .15rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface-subtle); color: var(--foreground-muted); font-size: .78rem; }
  .identity-note strong { color: var(--foreground); }
  code { font: .75rem 'JetBrains Mono Variable', monospace; }
  @media (max-width: 64rem) { .detail-grid { grid-template-columns: 1fr; } .diff-columns { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
  @media (max-width: 42rem) { .discovery-row, .revision-row { display: grid; } .diff-columns { grid-template-columns: 1fr; } }
</style>
