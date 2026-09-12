<script lang="ts">
  import ProviderBulkModels from './ProviderBulkModels.svelte';
  import { focusErrorSummary, focusFormError } from '$lib/forms/focusError';
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

<div class="wizard-body">
  {#if !wizard.canManage}
    <ReadOnlyNote>
      Your role can view providers but not connect or activate them.
    </ReadOnlyNote>
  {/if}
  {#if wizard.errorMessage}<div
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
      use:focusErrorSummary
    >
      {wizard.errorMessage}
      <ProviderValidationIssues issues={wizard.validationIssues} />
    </div>{/if}
  {#if wizard.notice}<div class="success-banner" role="status">
      {wizard.notice}
    </div>{/if}
  <p class="sr-only" role="status">
    {wizard.busy ? 'Operation in progress. Please wait.' : ''}
  </p>
  <ConflictNotice
    notice={wizard.wizardConflict ? 'conflict' : null}
    onReload={wizard.reloadWizard}
    disabled={Boolean(wizard.busy)}
  />

  {#if wizard.canManage}
    {#if wizard.wizardStep === 1}
      {#if wizard.providerKinds.isPending}
        <div class="card stage" role="status">
          Loading provider capabilities…
        </div>
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
          onSubmit={async (event) => {
            const root = (event.currentTarget as HTMLFormElement).closest(
              'main'
            );
            await wizard.createDraft(event);
            if (root) await focusFormError(root);
          }}
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
      <ProviderBulkModels
        provider={wizard.wizardProvider}
        canManage={wizard.canManage}
        onChanged={async () => {
          await wizard.refetchWizardModels();
        }}
      />
      <section class="card stage" aria-labelledby="capability-heading">
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
      </section>
      <div class="wizard-actions">
        <button
          class="button button-secondary"
          type="button"
          onclick={wizard.goBack}
          disabled={Boolean(wizard.busy)}>Back</button
        >
        <button
          class="button button-primary"
          type="button"
          disabled={Boolean(wizard.busy)}
          onclick={() => (wizard.wizardStep = 3)}>Continue to activation</button
        >
      </div>
    {:else if wizard.wizardStep === 3 && wizard.wizardProvider}
      <ProviderActivationStage
        provider={wizard.wizardProvider}
        activated={wizard.wizardProvider.state === 'active'}
        busy={wizard.busy}
        onBack={wizard.goBack}
        onTest={wizard.testWizardDraftForActivation}
        onActivate={wizard.activateWizardProvider}
        onAddAnother={wizard.startAnother}
      />
    {/if}
  {/if}
</div>

<style>
  .step-progress {
    display: none;
    margin: 2rem 0 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .wizard-body {
    display: grid;
    gap: 1rem;
    max-width: 66rem;
  }
  .wizard-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
  }
  /* The step list shares the wizard body's 66rem column so the stages line
     up beneath it. */
  .steps {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    max-width: 66rem;
    margin: 2rem 0 1.25rem;
    padding: 0;
    list-style: none;
  }
  .steps li {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.5rem;
    border-bottom: 1px solid var(--border-hairline);
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .steps li span {
    min-width: 1.25rem;
    font-variant-numeric: tabular-nums;
  }
  .steps li.current {
    color: var(--foreground);
  }
  .steps li.current::before {
    content: '';
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: var(--signal);
  }
  .steps li.complete {
    color: var(--metric);
  }
  .stage {
    padding: 1.5rem;
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

  @media (forced-colors: active) {
    .steps li.current::before {
      border: 1px solid CanvasText;
      background: CanvasText;
    }
  }
</style>
