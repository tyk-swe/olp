<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ProviderActivationStage from './ProviderActivationStage.svelte';
  import ProviderCapabilityReviewStage from './ProviderCapabilityReviewStage.svelte';
  import ProviderConnectorForm from './ProviderConnectorForm.svelte';
  import ProviderDiscoveryStage from './ProviderDiscoveryStage.svelte';
  import {
    invalidateProviderModelConsumers,
    invalidateProviderSummaries
  } from './providerCache';
  import { errorMessage as message, isEtagMismatch } from '$lib/api/http';
  import { emptyCursorHistory, resetCursor } from '$lib/api/pagination';
  import {
    activateProvider,
    certifyProviderModel,
    createProvider,
    declareProviderModels,
    discoverProviderModels,
    getProvider,
    getProviderCapabilityOptions,
    listProviderKinds,
    listProviderModelPage,
    probeProvider,
    setProviderModel,
    type CapabilityDeclaration,
    type CapabilityCertification,
    type Provider,
    type ProviderProbe
  } from '$lib/api/management/providers';
  import {
    authOptionsFor,
    buildCreateProviderInput,
    certificationPrerequisiteReady,
    createProviderDraft,
    parseManualModelNames,
    requiresCredential,
    validateProviderDraft,
    type ProviderDraft
  } from './providerEditor';

  const queryClient = useQueryClient();
  const providerKinds = createQuery(() => ({
    queryKey: ['provider-kinds'],
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  let draft = $state<ProviderDraft | null>(null);
  let wizardProvider = $state<Provider | null>(null);
  let wizardStep = $state(1);
  const wizardModelPagination = $state(emptyCursorHistory());
  const wizardModels = createQuery(() => ({
    queryKey: [
      'provider-model-page',
      wizardProvider?.id ?? '',
      wizardModelPagination.cursor ?? 'first'
    ],
    queryFn: ({ signal }) =>
      listProviderModelPage(
        wizardProvider!.id,
        wizardModelPagination.cursor,
        signal
      ),
    enabled: Boolean(wizardProvider) && wizardStep >= 3
  }));
  const capabilityOptions = createQuery(() => ({
    queryKey: ['provider-capability-options', wizardProvider?.kind ?? ''],
    queryFn: ({ signal }) =>
      getProviderCapabilityOptions(wizardProvider!.kind, signal),
    enabled: Boolean(wizardProvider)
  }));
  let probe = $state<ProviderProbe | null>(null);
  let manualModelNames = $state('');
  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>(
    {}
  );
  let wizardConflict = $state(false);
  let wizardModelReloadVersion = $state(0);

  const selectedSpec = $derived(
    providerKinds.data?.find((candidate) => candidate.kind === draft?.kind)
  );
  const authOptions = $derived(
    selectedSpec ? authOptionsFor(selectedSpec) : []
  );
  const credentialRequired = $derived(
    Boolean(
      draft && selectedSpec && requiresCredential(selectedSpec, draft.authMode)
    )
  );
  $effect(() => {
    const first = providerKinds.data?.[0];
    if (!draft && first) draft = createProviderDraft(first);
    const current = draft;
    const spec = selectedSpec;
    if (!current || !spec) return;
    if (!authOptions.some(([value]) => value === current.authMode)) {
      current.authMode = spec.default_auth_mode;
    }
    if (!credentialRequired) current.credential = '';
  });

  onDestroy(() => {
    if (draft) draft.credential = '';
  });

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
      if (wizardProvider && isEtagMismatch(error)) wizardConflict = true;
      else errorMessage = message(error);
      return false;
    } finally {
      busy = '';
    }
  }

  function clearCertificationResults() {
    certificationResults = {};
  }

  async function refetchWizardModels() {
    const result = await wizardModels.refetch();
    if (result.error) throw result.error;
    if (!result.data)
      throw new Error('The provider model reload returned no data.');
    return result.data;
  }

  async function reloadWizard() {
    if (!wizardProvider) return;
    busy = 'reload';
    errorMessage = '';
    notice = '';
    try {
      const reloadedProvider = await getProvider(wizardProvider.id);
      await refetchWizardModels();
      wizardProvider = reloadedProvider;
      wizardModelReloadVersion += 1;
      wizardConflict = false;
    } catch (error) {
      errorMessage = message(error);
    } finally {
      busy = '';
    }
  }

  async function createDraft(event: SubmitEvent) {
    event.preventDefault();
    if (!draft || !selectedSpec) return;
    const current = draft;
    const spec = selectedSpec;
    const issue = validateProviderDraft(current, spec);
    if (issue) {
      errorMessage = issue;
      return;
    }
    await run('create', async () => {
      const id = await createProvider(buildCreateProviderInput(current, spec));
      current.credential = '';
      wizardProvider = await getProvider(id);
      wizardStep = 2;
      await Promise.all([
        invalidateProviderSummaries(queryClient),
        invalidateProviderModelConsumers(queryClient)
      ]);
    });
  }

  async function testWizardProvider() {
    if (!wizardProvider) return;
    await run('probe', async () => {
      probe = await probeProvider(wizardProvider!);
      if (!probe.succeeded) throw new Error(probe.detail);
      wizardStep = 3;
    });
  }

  async function discoverWizardProvider() {
    if (!wizardProvider) return;
    await run('discover', async () => {
      const discovered = await discoverProviderModels(wizardProvider!);
      if (discovered.model_count === 0) {
        throw new Error(
          discovered.kind === 'openai_compatible'
            ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
            : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.'
        );
      }
      clearCertificationResults();
      resetCursor(wizardModelPagination);
      await refetchWizardModels();
      wizardProvider = discovered;
      wizardStep = 4;
      await invalidateProviderModelConsumers(queryClient);
    });
  }

  async function declareWizardModels() {
    if (!wizardProvider) return;
    const names = parseManualModelNames(manualModelNames);
    if (!names.length) {
      errorMessage = 'Enter at least one upstream model identifier.';
      return;
    }
    await run('declare-models', async () => {
      const declared = await declareProviderModels(wizardProvider!, names);
      clearCertificationResults();
      manualModelNames = '';
      resetCursor(wizardModelPagination);
      await refetchWizardModels();
      wizardProvider = declared;
      wizardStep = 4;
      await invalidateProviderModelConsumers(queryClient);
    });
  }

  async function reviewWizardModel(
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[],
    providerEtag: string
  ): Promise<boolean> {
    if (!wizardProvider) return false;
    return run(`model-${modelId}`, async () => {
      const updated = await setProviderModel(
        { ...wizardProvider!, etag: providerEtag },
        modelId,
        enabled,
        capabilities
      );
      clearCertificationResults();
      await refetchWizardModels();
      wizardProvider = updated;
      await invalidateProviderModelConsumers(queryClient);
      notice = 'Capability review saved with declared provenance.';
    });
  }

  async function certifyWizardModel(modelId: string) {
    if (!wizardProvider) return;
    await run(`certify-${modelId}`, async () => {
      if (!certificationPrerequisiteReady(wizardProvider!)) {
        probe = await probeProvider(wizardProvider!);
        if (!probe.succeeded) throw new Error(probe.detail);
        wizardProvider = {
          ...wizardProvider!,
          last_probe_at: probe.checked_at,
          last_probe_status: 'succeeded',
          last_probe_detail: probe.detail
        };
      }
      const result = await certifyProviderModel(wizardProvider!, modelId);
      certificationResults = { ...certificationResults, [modelId]: result };
      const updated = await getProvider(wizardProvider!.id);
      await refetchWizardModels();
      wizardProvider = updated;
      await invalidateProviderModelConsumers(queryClient);
      probe = null;
      notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
    });
  }

  async function testWizardDraftForActivation() {
    if (!wizardProvider) return;
    await run('final-probe', async () => {
      probe = await probeProvider(wizardProvider!);
      if (!probe.succeeded) throw new Error(probe.detail);
      wizardProvider = await getProvider(wizardProvider!.id);
      notice = `Final draft test passed: ${probe.detail}`;
    });
  }

  async function activateWizardProvider() {
    if (!wizardProvider) return;
    await run('activate', async () => {
      const generation = await activateProvider(wizardProvider!);
      wizardProvider = await getProvider(wizardProvider!.id);
      wizardStep = 5;
      notice = `Provider activated in runtime generation ${generation}.`;
      await Promise.all([
        invalidateProviderSummaries(queryClient),
        invalidateProviderModelConsumers(queryClient)
      ]);
    });
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway · Provider wizard</p>
    <h1 class="page-title">Connect an upstream provider.</h1>
    <p class="page-description">
      Credentials are write-only. Test reachability, review model capabilities,
      then activate.
    </p>
  </div>
  <a class="button button-secondary" href={resolve('/providers')}>Cancel</a>
</div>

<ol class="steps" aria-label="Provider setup progress">
  {#each ['Connector', 'Test', 'Discovery', 'Capabilities', 'Activate'] as label, index (label)}
    <li
      class:current={wizardStep === index + 1}
      class:complete={wizardStep > index + 1}
      aria-current={wizardStep === index + 1 ? 'step' : undefined}
    >
      <span>{wizardStep > index + 1 ? '✓' : index + 1}</span>{label}
    </li>
  {/each}
</ol>

{#if errorMessage}<div class="inline-problem" role="alert">
    {errorMessage}
  </div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}
<ConflictNotice
  notice={wizardConflict ? 'conflict' : null}
  onReload={reloadWizard}
  disabled={Boolean(busy)}
/>

{#if wizardStep === 1 && providerKinds.isPending}
  <div class="card stage" role="status">Loading provider capabilities…</div>
{:else if wizardStep === 1 && providerKinds.isError}
  <div class="inline-problem" role="alert">
    Provider capabilities could not be loaded. Retry before configuring a
    provider. <button
      class="button button-secondary"
      type="button"
      onclick={() => providerKinds.refetch()}>Retry</button
    >
  </div>
{:else if wizardStep === 1 && draft && selectedSpec}
  <ProviderConnectorForm
    bind:draft
    providerKinds={providerKinds.data ?? []}
    {selectedSpec}
    {busy}
    onSubmit={createDraft}
  />
{:else if wizardStep === 2}
  <section class="card stage" aria-labelledby="test-heading">
    <p class="eyebrow">Identity saved</p>
    <h2 id="test-heading">Verify upstream reachability</h2>
    <p>
      The control plane performs a bounded connector-specific probe. No client
      request content is sent.
    </p>
    <dl>
      <div>
        <dt>Provider</dt>
        <dd>{wizardProvider?.name}</dd>
      </div>
      <div>
        <dt>Connector</dt>
        <dd>{wizardProvider?.kind}</dd>
      </div>
    </dl>
    <button
      class="button button-primary"
      type="button"
      onclick={testWizardProvider}
      disabled={Boolean(busy)}
      >{busy === 'probe' ? 'Testing…' : 'Test connection'}</button
    >
  </section>
{:else if wizardStep === 3}
  <ProviderDiscoveryStage
    provider={wizardProvider}
    {probe}
    bind:manualModelNames
    {busy}
    onDiscover={discoverWizardProvider}
    onDeclareModels={declareWizardModels}
  />
{:else if wizardStep === 4 && wizardProvider}
  <section class="card stage wide" aria-labelledby="capability-heading">
    <ProviderCapabilityReviewStage
      provider={wizardProvider}
      models={wizardModels.data?.items ?? []}
      capabilityOptions={capabilityOptions.data?.capabilities ?? []}
      optionsPending={capabilityOptions.isPending}
      optionsError={capabilityOptions.isError}
      {busy}
      reloadVersion={wizardModelReloadVersion}
      {certificationResults}
      pagination={wizardModelPagination}
      nextCursor={wizardModels.data?.nextCursor}
      onSave={reviewWizardModel}
      onCertify={certifyWizardModel}
    />
    <ProviderActivationStage
      provider={wizardProvider}
      {busy}
      onTest={testWizardDraftForActivation}
      onActivate={activateWizardProvider}
    />
  </section>
{:else}
  <ProviderActivationStage
    provider={wizardProvider}
    activated
    {busy}
    onTest={testWizardDraftForActivation}
    onActivate={activateWizardProvider}
  />
{/if}

<style>
  .steps {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    max-width: 58rem;
    margin: 2rem 0 1.25rem;
    padding: 0;
    list-style: none;
  }
  .steps li {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    border-bottom: 2px solid var(--border);
    color: var(--foreground-muted);
    font-size: 0.78rem;
    font-weight: 700;
  }
  .steps li span {
    display: grid;
    width: 1.6rem;
    height: 1.6rem;
    place-items: center;
    border: 1px solid var(--border-strong);
    border-radius: 50%;
    font: 700 0.68rem 'JetBrains Mono Variable';
  }
  .steps li.current,
  .steps li.complete {
    border-color: var(--accent);
    color: var(--foreground);
  }
  .steps li.current span {
    border-color: var(--accent);
    background: var(--accent-soft);
    color: var(--accent-strong);
  }
  .steps li.complete span {
    border-color: transparent;
    background: var(--success-soft);
    color: var(--success);
  }
  .stage {
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
    max-width: 48rem;
  }
  .stage.wide {
    max-width: none;
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .stage > p {
    color: var(--foreground-muted);
  }
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  dl {
    display: grid;
    grid-template-columns: repeat(2, 1fr);
    gap: 0.75rem;
    margin: 1rem 0;
  }
  dl div {
    padding: 0.75rem;
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }
  dt {
    color: var(--foreground-muted);
    font-size: 0.72rem;
  }
  dd {
    margin: 0.15rem 0 0;
    font-weight: 700;
  }
  @media (max-width: 42rem) {
    .steps {
      grid-template-columns: 1fr;
    }
    .steps li:not(.current) {
      display: none;
    }
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
