<script lang="ts">
  import { resolve } from '$app/paths';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import ProviderActivationStage from '$lib/features/providers/ProviderActivationStage.svelte';
  import ProviderCapabilityReviewStage from '$lib/features/providers/ProviderCapabilityReviewStage.svelte';
  import ProviderConnectorForm from '$lib/features/providers/ProviderConnectorForm.svelte';
  import ProviderDiscoveryStage from '$lib/features/providers/ProviderDiscoveryStage.svelte';
  import ProviderValidationIssues from '$lib/features/providers/ProviderValidationIssues.svelte';
  import { ProviderWizardState } from '$lib/features/providers/providerWizard.svelte';

  const wizard = new ProviderWizardState();
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
  <a class="button button-secondary" href={resolve('/providers')}
    >{wizard.wizardProvider ? 'Save and exit' : 'Cancel'}</a
  >
</div>

<p class="step-progress" aria-hidden="true">
  Step {wizard.wizardStep} of {wizard.stepLabels.length}
</p>
<ol class="steps" aria-label="Provider setup progress">
  {#each wizard.stepLabels as label, index (label)}
    <li
      class:current={wizard.wizardStep === index + 1}
      class:complete={wizard.wizardStep > index + 1}
      aria-current={wizard.wizardStep === index + 1 ? 'step' : undefined}
    >
      <span>{wizard.wizardStep > index + 1 ? '✓' : index + 1}</span>{label}
    </li>
  {/each}
</ol>

{#if !wizard.canManage}
  <ReadOnlyNote>
    Your role can view providers but not connect or activate them.
  </ReadOnlyNote>
{/if}
{#if wizard.canManage && wizard.wizardStep >= 2 && wizard.wizardStep <= 3}
  <div class="wizard-back">
    <button
      class="button button-secondary"
      type="button"
      onclick={wizard.goBack}
      disabled={Boolean(wizard.busy)}>Back</button
    >
  </div>
{/if}
{#if wizard.errorMessage}<div class="inline-problem" role="alert">
    {wizard.errorMessage}
    <ProviderValidationIssues issues={wizard.validationIssues} />
  </div>{/if}
{#if wizard.notice}<div class="success-banner" role="status">
    {wizard.notice}
  </div>{/if}
<ConflictNotice
  notice={wizard.wizardConflict ? 'conflict' : null}
  onReload={wizard.reloadWizard}
  disabled={Boolean(wizard.busy)}
/>

{#if wizard.canManage}
  {#if wizard.wizardStep === 1}
    {#if wizard.providerKinds.isPending}
      <div class="card stage" role="status">Loading provider capabilities…</div>
    {:else if wizard.providerKinds.isError}
      <div class="inline-problem" role="alert">
        Provider capabilities could not be loaded. Retry before configuring a
        provider. <button
          class="button button-secondary"
          type="button"
          onclick={() => wizard.providerKinds.refetch()}>Retry</button
        >
      </div>
    {:else if wizard.draft && wizard.selectedSpec}
      <ProviderConnectorForm
        bind:draft={wizard.draft}
        providerKinds={wizard.providerKinds.data ?? []}
        selectedSpec={wizard.selectedSpec}
        busy={wizard.busy}
        lockKind={Boolean(wizard.wizardProvider)}
        onSubmit={wizard.createDraft}
      />
    {/if}
  {:else if wizard.wizardStep === 2 && wizard.wizardProvider}
    <ProviderDiscoveryStage
      provider={wizard.wizardProvider}
      probe={wizard.probe}
      bind:manualModelNames={wizard.manualModelNames}
      busy={wizard.busy}
      onDiscover={wizard.discoverWizardProvider}
      onDeclareModels={wizard.declareWizardModels}
    />
    <section class="card stage wide" aria-labelledby="capability-heading">
      <ProviderCapabilityReviewStage
        provider={wizard.wizardProvider}
        models={wizard.wizardModels.data?.items ?? []}
        modelsPending={wizard.wizardModels.isPending}
        modelsError={wizard.wizardModels.isError}
        capabilityOptions={wizard.capabilityOptions.data?.capabilities ?? []}
        optionsPending={wizard.capabilityOptions.isPending}
        optionsError={wizard.capabilityOptions.isError}
        busy={wizard.busy}
        reloadVersion={wizard.wizardModelReloadVersion}
        certificationResults={wizard.certificationResults}
        pagination={wizard.wizardModelPagination}
        nextCursor={wizard.wizardModels.data?.nextCursor}
        onSave={wizard.reviewWizardModel}
        onCertify={wizard.certifyWizardModel}
        onRetryModels={() => wizard.refetchWizardModels()}
      />
      <button
        class="button button-primary"
        type="button"
        disabled={Boolean(wizard.busy)}
        onclick={() => (wizard.wizardStep = 3)}>Continue to activation</button
      >
    </section>
  {:else if wizard.wizardStep === 3 && wizard.wizardProvider}
    <ProviderActivationStage
      provider={wizard.wizardProvider}
      activated={wizard.wizardProvider.state === 'active'}
      busy={wizard.busy}
      onTest={wizard.testWizardDraftForActivation}
      onActivate={wizard.activateWizardProvider}
    />
  {/if}
{/if}

<style>
  .step-progress {
    display: none;
    margin: 2rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
    font-weight: 700;
  }
  .wizard-back {
    margin-top: 1rem;
  }
  .steps {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
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
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  @media (max-width: 42rem) {
    .step-progress {
      display: block;
    }
    .steps {
      grid-template-columns: 1fr;
      margin-top: 0.4rem;
    }
    .steps li:not(.current) {
      display: none;
    }
  }
</style>
