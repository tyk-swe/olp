<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
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
    createProvider,
    declareProviderModels,
    discoverProviderModels,
    getProvider,
    getProviderCapabilityOptions,
    listProviderModelPage,
    probeProvider,
    setProviderModel,
    type CapabilityDeclaration,
    type CapabilityCertification,
    type Provider,
    type ProviderProbe
  } from '$lib/api/management/providers';
  import { connectorOptions } from './providerOptions';
  import {
    activationReady,
    authOptionsFor,
    buildCreateProviderInput,
    capabilitiesCertified,
    createProviderDraft,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    isCompatibleEndpoint,
    parseManualModelNames,
    probeReady,
    requiresCredential,
    requiresSeedModel,
    validateProviderDraft
  } from './providerEditor';

  const queryClient = useQueryClient();
  let draft = $state(createProviderDraft());
  let wizardProvider = $state<Provider | null>(null);
  let wizardStep = $state(1);
  let wizardModelCursor = $state<string | undefined>();
  let wizardModelHistory = $state<Array<string | undefined>>([]);
  const wizardModels = createQuery(() => ({
    queryKey: ['provider-model-page', wizardProvider?.id ?? '', wizardModelCursor ?? 'first'],
    queryFn: ({ signal }) => listProviderModelPage(wizardProvider!.id, wizardModelCursor, signal),
    enabled: Boolean(wizardProvider) && wizardStep >= 3
  }));
  const capabilityOptions = createQuery(() => ({
    queryKey: ['provider-capability-options', wizardProvider?.kind ?? ''],
    queryFn: ({ signal }) => getProviderCapabilityOptions(wizardProvider!.kind, signal),
    enabled: Boolean(wizardProvider)
  }));
  let probe = $state<ProviderProbe | null>(null);
  let manualModelNames = $state('');
  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>({});

  const authOptions = $derived(authOptionsFor(draft.kind));
  const credentialRequired = $derived(requiresCredential(draft.authMode));
  const seedModelRequired = $derived(requiresSeedModel(draft.kind));

  $effect(() => {
    if (!authOptions.some(([value]) => value === draft.authMode)) draft.authMode = authOptions[0][0];
    if (!credentialRequired) draft.credential = '';
  });

  onDestroy(() => {
    draft.credential = '';
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

  function nextWizardModelPage() {
    const next = wizardModels.data?.nextCursor;
    if (!next) return;
    wizardModelHistory = [...wizardModelHistory, wizardModelCursor];
    wizardModelCursor = next;
  }

  function previousWizardModelPage() {
    wizardModelCursor = wizardModelHistory.at(-1);
    wizardModelHistory = wizardModelHistory.slice(0, -1);
  }

  async function createDraft(event: SubmitEvent) {
    event.preventDefault();
    const issue = validateProviderDraft(draft);
    if (issue) {
      errorMessage = issue;
      return;
    }
    await run('create', async () => {
      const id = await createProvider(buildCreateProviderInput(draft));
      draft.credential = '';
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
          isCompatibleEndpoint(discovered.kind)
            ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
            : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.'
        );
      }
      wizardProvider = discovered;
      clearCertificationResults();
      wizardModelCursor = undefined;
      wizardModelHistory = [];
      await wizardModels.refetch();
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
      wizardProvider = await declareProviderModels(wizardProvider!, names);
      if (wizardProvider.kind === 'anthropic_compatible') {
        probe = await probeProvider(wizardProvider, names[0]);
      }
      clearCertificationResults();
      manualModelNames = '';
      wizardModelCursor = undefined;
      wizardModelHistory = [];
      await wizardModels.refetch();
      wizardStep = 4;
      await invalidateProviderModelConsumers(queryClient);
    });
  }

  async function reviewWizardModel(
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) {
    if (!wizardProvider) return;
    await run(`model-${modelId}`, async () => {
      wizardProvider = await setProviderModel(wizardProvider!, modelId, enabled, capabilities);
      clearCertificationResults();
      await wizardModels.refetch();
      await invalidateProviderModelConsumers(queryClient);
      notice = 'Capability review saved with declared provenance.';
    });
  }

  async function certifyWizardModel(modelId: string) {
    if (!wizardProvider) return;
    await run(`certify-${modelId}`, async () => {
      const result = await certifyProviderModel(wizardProvider!, modelId);
      certificationResults = { ...certificationResults, [modelId]: result };
      wizardProvider = await getProvider(wizardProvider!.id);
      await wizardModels.refetch();
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
    <p class="page-description">Credentials are write-only. Test reachability, review model capabilities, then activate.</p>
  </div>
  <a class="button button-secondary" href="/providers">Cancel</a>
</div>

<ol class="steps" aria-label="Provider setup progress">
  {#each ['Connector', 'Test', 'Discovery', 'Capabilities', 'Activate'] as label, index (label)}
    <li class:current={wizardStep === index + 1} class:complete={wizardStep > index + 1} aria-current={wizardStep === index + 1 ? 'step' : undefined}>
      <span>{wizardStep > index + 1 ? '✓' : index + 1}</span>{label}
    </li>
  {/each}
</ol>

{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if wizardStep === 1}
  <form class="card editor" onsubmit={createDraft} novalidate>
    <fieldset>
      <legend>Choose a connector</legend>
      <div class="connector-grid">
        {#each connectorOptions as option (option[0])}
          <label class:selected={draft.kind === option[0]}>
            <input type="radio" name="kind" value={option[0]} bind:group={draft.kind} />
            <strong>{option[1]}</strong><small>{option[2]}</small>
          </label>
        {/each}
      </div>
    </fieldset>
    <div class="form-grid">
      <div class="form-field"><label for="provider-name">Provider name</label><input id="provider-name" autocomplete="off" bind:value={draft.name} placeholder="production-openai" required /></div>
      <div class="form-field"><label for="auth-mode">Authentication</label><select id="auth-mode" bind:value={draft.authMode}>{#each authOptions as option (option[0])}<option value={option[0]}>{option[1]}</option>{/each}</select></div>
      <div class="form-field"><label for="initial-model">{seedModelRequired ? 'Vertex probe model' : 'Seed model (optional)'}</label><input id="initial-model" autocomplete="off" bind:value={draft.model} placeholder={seedModelRequired ? 'publishers/google/models/gemini-2.5-pro' : 'gpt-5.4'} required={seedModelRequired} /><small>{seedModelRequired ? 'Vertex requires a publisher model because it has no global model-list operation.' : 'Used for the initial connector probe; upstream discovery follows.'}</small></div>
      {#if hasCustomEndpoint(draft.kind)}<div class="form-field full"><label for="provider-endpoint">{draft.kind === 'azure_open_ai' ? 'Azure resource endpoint' : draft.kind === 'anthropic_compatible' ? 'Anthropic-compatible endpoint' : 'OpenAI-compatible endpoint'}</label><input id="provider-endpoint" type="url" autocomplete="off" bind:value={draft.endpoint} placeholder={draft.kind === 'azure_open_ai' ? 'https://resource.openai.azure.com' : 'https://models.example.com/v1'} required /><small>Custom endpoints must be HTTPS and pass the gateway SSRF policy.</small></div>{/if}
      {#if hasApiVersion(draft.kind)}<div class="form-field"><label for="api-version">API version</label><input id="api-version" autocomplete="off" bind:value={draft.apiVersion} required /></div>{/if}
      {#if hasCloudRegion(draft.kind)}<div class="form-field"><label for="cloud-region">Cloud region</label><input id="cloud-region" autocomplete="off" bind:value={draft.cloudRegion} placeholder="us-east-1" required /></div>{/if}
      {#if hasCloudProject(draft.kind)}<div class="form-field"><label for="cloud-project">Cloud project</label><input id="cloud-project" autocomplete="off" bind:value={draft.cloudProject} placeholder="my-gcp-project" required /></div>{/if}
      {#if hasDeployment(draft.kind)}<div class="form-field"><label for="deployment">Cloud deployment</label><input id="deployment" autocomplete="off" bind:value={draft.deployment} placeholder="Azure deployment name" required /></div>{/if}
      {#if credentialRequired}<div class="form-field full"><label for="provider-secret">Credential</label><input id="provider-secret" type="password" autocomplete="new-password" bind:value={draft.credential} required /><small>Sent once to this installation; never saved by the console or returned by the API.</small></div>{:else}<div class="identity-note full"><strong>No stored credential</strong><span>This provider uses the workload identity available to the OLP process.</span></div>{/if}
    </div>
    <div class="form-actions"><button class="button button-primary" type="submit" disabled={Boolean(busy)}>{busy === 'create' ? 'Saving securely…' : 'Save and test connection'} <NavIcon name="arrow" /></button></div>
  </form>
{:else if wizardStep === 2}
  <section class="card stage" aria-labelledby="test-heading">
    <p class="eyebrow">Identity saved</p><h2 id="test-heading">Verify upstream reachability</h2>
    <p>The control plane performs a bounded connector-specific probe. No client request content is sent.</p>
    <dl><div><dt>Provider</dt><dd>{wizardProvider?.name}</dd></div><div><dt>Connector</dt><dd>{wizardProvider?.kind}</dd></div></dl>
    <button class="button button-primary" type="button" onclick={testWizardProvider} disabled={Boolean(busy)}>{busy === 'probe' ? 'Testing…' : 'Test connection'}</button>
    {#if wizardProvider && isCompatibleEndpoint(wizardProvider.kind)}<details class="manual-fallback"><summary>Endpoint has no model-list API?</summary><p>Declare identifiers manually to test a replacement model or continue capability review. Anthropic-compatible endpoints retry their bounded Messages request against the first declared model.</p><div class="form-field"><label for="manual-models-wizard-test">Upstream model identifiers</label><textarea id="manual-models-wizard-test" bind:value={manualModelNames} placeholder="model-a&#10;model-b"></textarea></div><button class="button button-secondary" type="button" onclick={declareWizardModels} disabled={Boolean(busy)}>{busy === 'declare-models' ? 'Adding…' : 'Add identifiers for review'}</button></details>{/if}
  </section>
{:else if wizardStep === 3}
  <section class="card stage" aria-labelledby="discovery-heading">
    <p class="eyebrow">Probe passed</p><h2 id="discovery-heading">Discover upstream models</h2>
    {#if probe}<p class="success-line">✓ {probe.detail}</p>{/if}
    <p>The connector will call the upstream model-list API with the stored identity. Discovered models begin disabled until their capabilities are certified and reviewed.</p>
    <button class="button button-primary" type="button" onclick={discoverWizardProvider} disabled={Boolean(busy)}>{busy === 'discover' ? 'Discovering…' : 'Discover upstream models'}</button>
    {#if wizardProvider && isCompatibleEndpoint(wizardProvider.kind)}<details class="manual-fallback"><summary>Endpoint has no model-list API?</summary><p>Declare identifiers manually. Anthropic-compatible endpoints retry their bounded Messages request against the first declared model; models remain disabled and capability-empty until you complete review.</p><div class="form-field"><label for="manual-models-wizard">Upstream model identifiers</label><textarea id="manual-models-wizard" bind:value={manualModelNames} placeholder="model-a&#10;model-b"></textarea></div><button class="button button-secondary" type="button" onclick={declareWizardModels} disabled={Boolean(busy)}>{busy === 'declare-models' ? 'Adding…' : 'Add identifiers for review'}</button></details>{/if}
  </section>
{:else if wizardStep === 4}
  <section class="card stage wide" aria-labelledby="capability-heading">
    <p class="eyebrow">Capability review</p><h2 id="capability-heading">Review model capabilities</h2>
    <p>Operator review is recorded as <code>declared</code>. Every native and compatible capability tuple must receive server-owned certification for this exact draft.</p>
    <div class="table-shell"><table class="data-table"><thead><tr><th>Model</th><th>Explicit capability review</th></tr></thead><tbody>
      {#each wizardModels.data?.items ?? [] as model (model.id)}
        <tr><td><strong>{model.display_name}</strong><br /><code>{model.upstream_model}</code></td><td><CapabilityReview {model} options={capabilityOptions.data?.capabilities ?? []} optionsPending={capabilityOptions.isPending} optionsError={capabilityOptions.isError} disabled={Boolean(busy)} onSave={(enabled, capabilities) => reviewWizardModel(model.id, enabled, capabilities)} /><div class="certification-action"><button class="button button-secondary" type="button" onclick={() => certifyWizardModel(model.id)} disabled={Boolean(busy) || !model.capabilities.length}>{busy === `certify-${model.id}` ? 'Server-certifying…' : 'Server-certify capabilities'}</button>{#if certificationResults[model.id]}{@const result = certificationResults[model.id]}<span class:success={result.status === 'succeeded'} class:warning={result.status !== 'succeeded'}>{result.certified_count}/{result.attempted_count} certified</span>{/if}</div></td></tr>
      {/each}
    </tbody></table></div>
    <CursorPagination page={wizardModelHistory.length + 1} hasPrevious={wizardModelHistory.length > 0} hasNext={Boolean(wizardModels.data?.nextCursor)} onPrevious={previousWizardModelPage} onNext={nextWizardModelPage} label="Provider wizard model pages" />
    <ol class="activation-checklist" aria-label="Provider activation requirements"><li class:complete={capabilitiesCertified(wizardProvider)}>{capabilitiesCertified(wizardProvider) ? '✓' : '1'} Every enabled capability is server-certified</li><li class:complete={probeReady(wizardProvider)}>{probeReady(wizardProvider) ? '✓' : '2'} Completed draft passed an ETag-bound connection test</li></ol>
    <div class="form-actions"><button class="button button-secondary" type="button" onclick={testWizardDraftForActivation} disabled={Boolean(busy) || !capabilitiesCertified(wizardProvider)}>{busy === 'final-probe' ? 'Testing completed draft…' : 'Test completed draft'}</button><button class="button button-primary" type="button" onclick={activateWizardProvider} disabled={Boolean(busy) || !activationReady(wizardProvider)}>{busy === 'activate' ? 'Activating…' : 'Activate provider'}</button></div>
    {#if wizardProvider && !activationReady(wizardProvider)}<p class="audit-note">Activation stays disabled until every tuple has server-owned certification and the completed draft passes a fresh connection test. Any configuration, credential, discovery, or capability change invalidates that evidence.</p>{/if}
  </section>
{:else}
  <section class="card stage complete-panel" aria-labelledby="activated-heading">
    <span class="complete-mark" aria-hidden="true">✓</span><p class="eyebrow">Provider active</p><h2 id="activated-heading">Now build a stable route slug.</h2>
    <p>{wizardProvider?.name} is eligible for new route drafts. Activation published an immutable runtime generation.</p>
    <div class="form-actions"><a class="button button-primary" href="/routes/new">Build default route <NavIcon name="arrow" /></a><a class="button button-secondary" href={`/providers/${wizardProvider?.id}`}>View provider</a></div>
  </section>
{/if}

<style>
  .steps { display: grid; grid-template-columns: repeat(5, 1fr); max-width: 58rem; margin: 2rem 0 1.25rem; padding: 0; list-style: none; }
  .steps li { display: flex; min-height: 2.75rem; align-items: center; gap: .45rem; border-bottom: 2px solid var(--border); color: var(--foreground-muted); font-size: .78rem; font-weight: 700; }
  .steps li span { display: grid; width: 1.6rem; height: 1.6rem; place-items: center; border: 1px solid var(--border-strong); border-radius: 50%; font: 700 .68rem 'JetBrains Mono Variable'; }
  .steps li.current, .steps li.complete { border-color: var(--accent); color: var(--foreground); }
  .steps li.current span { border-color: var(--accent); background: var(--accent-soft); color: var(--accent-strong); }
  .steps li.complete span { border-color: transparent; background: var(--success-soft); color: var(--success); }
  .editor, .stage { max-width: 66rem; margin-top: 1.25rem; padding: clamp(1.15rem, 3vw, 1.75rem); }
  .stage { max-width: 48rem; }
  .stage.wide { max-width: none; }
  fieldset { margin: 0 0 1.5rem; padding: 0; border: 0; }
  legend, h2 { margin: 0 0 .85rem; font-size: 1.15rem; font-weight: 750; letter-spacing: -.025em; }
  .connector-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .6rem; }
  .connector-grid label { display: grid; min-height: 5.6rem; align-content: center; gap: .2rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; }
  .connector-grid label.selected { border-color: var(--accent); background: var(--accent-soft); }
  .connector-grid input { position: absolute; opacity: 0; }
  .connector-grid small, .audit-note, .stage > p { color: var(--foreground-muted); }
  .form-actions { display: flex; flex-wrap: wrap; gap: .65rem; margin-top: 1.35rem; }
  .success-banner { margin: 1rem 0; padding: .85rem 1rem; border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border)); border-radius: .375rem; background: var(--success-soft); color: var(--success); }
  .success-line { color: var(--success) !important; font-weight: 700; }
  dl { display: grid; grid-template-columns: repeat(2, 1fr); gap: .75rem; margin: 1rem 0; }
  dl div { padding: .75rem; border-radius: .375rem; background: var(--surface-subtle); } dt { color: var(--foreground-muted); font-size: .72rem; } dd { margin: .15rem 0 0; font-weight: 700; }
  .complete-panel { text-align: center; } .complete-mark { display: grid; width: 3rem; height: 3rem; place-items: center; margin: 0 auto 1rem; border-radius: 50%; background: var(--success-soft); color: var(--success); font-size: 1.4rem; font-weight: 800; }
  .complete-panel .form-actions { justify-content: center; }
  .certification-action { display: flex; flex-wrap: wrap; align-items: center; gap: .6rem; margin-top: .75rem; padding-top: .75rem; border-top: 1px solid var(--border); color: var(--foreground-muted); font-size: .75rem; }
  .activation-checklist { display: grid; gap: .4rem; margin: 1rem 0 0; padding: 0; list-style: none; color: var(--foreground-muted); font-size: .8rem; }
  .activation-checklist li { min-height: 1.5rem; }
  .activation-checklist li.complete { color: var(--success); font-weight: 700; }
  .manual-fallback { margin-top: 1rem; padding: .75rem; border: 1px solid var(--border); border-radius: .375rem; }
  .manual-fallback summary { min-height: 2.75rem; font-weight: 720; }
  .manual-fallback p { color: var(--foreground-muted); font-size: .78rem; }
  .manual-fallback textarea { min-height: 5rem; }
  .identity-note { display: grid; gap: .15rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface-subtle); color: var(--foreground-muted); font-size: .78rem; }
  .identity-note strong { color: var(--foreground); }
  .identity-note.full { grid-column: 1 / -1; }
  code { font: .75rem 'JetBrains Mono Variable', monospace; }
  @media (max-width: 64rem) { .connector-grid { grid-template-columns: repeat(2, 1fr); } }
  @media (max-width: 42rem) { .steps { grid-template-columns: 1fr; } .steps li:not(.current) { display: none; } .connector-grid, dl { grid-template-columns: 1fr; } }
</style>
