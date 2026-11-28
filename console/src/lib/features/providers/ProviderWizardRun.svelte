<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import ProviderBulkModels from './ProviderBulkModels.svelte';
  import {
    focusErrorSummary,
    focusFormError
  } from '#lib/editing/focusError.js';
  import { resolve } from '$app/paths';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import { untrack } from 'svelte';
  import ProviderActivationStage from '#lib/features/providers/ProviderActivationStage.svelte';
  import ProviderCapabilityReviewStage from '#lib/features/providers/ProviderCapabilityReviewStage.svelte';
  import ProviderConnectorForm from '#lib/features/providers/ProviderConnectorForm.svelte';
  import ProviderDiscoveryStage from '#lib/features/providers/ProviderDiscoveryStage.svelte';
  import GrantEnrollmentPanel from '#lib/features/providers/GrantEnrollmentPanel.svelte';
  import ProviderValidationIssues from '#lib/features/providers/ProviderValidationIssues.svelte';
  import { useProviderWizard } from '#lib/features/providers/wizard/wizard.svelte.js';

  let {
    copyFrom,
    onAddAnother
  }: { copyFrom: string | null; onAddAnother: () => void } = $props();

  const wizard = useProviderWizard(untrack(() => copyFrom));
  const { operation, saved, form, connection, models, activation } = wizard;
  const stepLabels = [
    'Connection',
    'Models and capabilities',
    'Activation'
  ] as const;
</script>

<PageHeader
  title="Add a provider"
  description="Credentials are write-only. Test reachability, review model capabilities, then activate."
>
  {#snippet actions()}
    <a class="button button-secondary" href={resolve('providers')}>
      {saved.provider ? 'Save and exit' : 'Cancel'}
    </a>
  {/snippet}
</PageHeader>
<p class="step-progress" aria-hidden="true">
  Step {wizard.step} of {stepLabels.length}
</p>
<ol class="steps" aria-label="Provider setup progress">
  {#each stepLabels as label, index (label)}
    <li
      class:current={wizard.step === index + 1}
      class:complete={wizard.step > index + 1}
      aria-current={wizard.step === index + 1 ? 'step' : undefined}
    >
      <span>{wizard.step > index + 1 ? '✓' : index + 1}</span>{label}
    </li>
  {/each}
</ol>

<div class="wizard-body">
  {#if !wizard.canManage}
    <ReadOnlyNote>
      Your role can view providers but not connect or activate them.
    </ReadOnlyNote>
  {/if}
  {#if operation.error}<div
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
      use:focusErrorSummary
    >
      {operation.error}
      <ProviderValidationIssues issues={operation.issues} />
    </div>{/if}
  {#if operation.notice}<Callout tone="success">
      {operation.notice}
    </Callout>{/if}
  <p class="sr-only" role="status">
    {wizard.busy ? 'Operation in progress. Please wait.' : ''}
  </p>
  <ConflictNotice
    notice={operation.conflict ? 'conflict' : null}
    onReload={wizard.reload}
    disabled={Boolean(wizard.busy)}
  />

  {#if wizard.canManage}
    {#if wizard.step === 1}
      {#if form.kinds.isPending}
        <div class="card stage" role="status">
          Loading provider capabilities…
        </div>
      {:else if form.kinds.isError}
        <div class="inline-problem" role="alert">
          Provider capabilities could not be loaded. Retry before configuring a
          provider. <button
            class="button button-secondary"
            type="button"
            onclick={() => form.kinds.refetch()}
          >
            Retry
          </button>
        </div>
      {:else if form.draft && form.spec}
        <ProviderConnectorForm
          bind:draft={form.draft}
          providerKinds={form.kinds.data ?? []}
          selectedSpec={form.spec}
          busy={wizard.busy}
          lockKind={Boolean(saved.provider)}
          errors={operation.fieldErrors}
          issues={operation.issues}
          grantEnrolled={connection.grantEnrolled}
          onSubmit={async (event) => {
            event.preventDefault();
            const root = (event.currentTarget as HTMLFormElement).closest(
              'main'
            );
            await connection.submit();
            if (root) await focusFormError(root);
          }}
        />
        {#if connection.grant.enrollment}<GrantEnrollmentPanel
            profileId={saved.provider?.configuration.profile_id}
            enrollment={connection.grant.enrollment}
            bind:input={connection.grant.input}
            busy={wizard.busy}
            onContinue={() => connection.grant.continue()}
            onPoll={() => connection.grant.poll()}
            onCancel={() => connection.grant.cancel()}
          />{/if}
      {/if}
    {:else if wizard.step === 2 && saved.provider}
      <ProviderDiscoveryStage
        provider={saved.provider}
        probe={saved.probe}
        bind:manualModelNames={models.manualNames}
        error={operation.fieldErrors.manualModelNames}
        busy={wizard.busy}
        onDiscover={models.discover}
        onDeclareModels={models.declare}
      />
      <ProviderBulkModels
        provider={saved.provider}
        canManage={wizard.canManage}
        onChanged={async () => {
          await saved.refetch();
        }}
      />
      <section class="card stage" aria-labelledby="capability-heading">
        <ProviderCapabilityReviewStage
          provider={saved.provider}
          models={saved.models.data?.items ?? []}
          modelsPending={saved.models.isPending}
          modelsError={saved.models.isError}
          capabilityOptions={saved.capabilityOptions.data?.capabilities ?? []}
          optionsPending={saved.capabilityOptions.isPending}
          optionsError={saved.capabilityOptions.isError}
          busy={wizard.busy}
          reloadVersion={wizard.reviewVersion}
          certificationResults={models.certificationResults}
          pagination={saved.pagination}
          nextCursor={saved.models.data?.nextCursor}
          onSave={models.review}
          onCertify={models.certify}
          onRetryModels={() => saved.refetch()}
        />
      </section>
      <div class="wizard-actions">
        <button
          class="button button-secondary"
          type="button"
          onclick={wizard.goBack}
          disabled={Boolean(wizard.busy)}
        >
          Back
        </button>
        <button
          class="button button-primary"
          type="button"
          disabled={Boolean(wizard.busy)}
          onclick={wizard.continueToActivation}
        >
          Continue to activation
        </button>
      </div>
    {:else if wizard.step === 3 && saved.provider}
      <ProviderActivationStage
        provider={saved.provider}
        activated={saved.provider.state === 'active'}
        busy={wizard.busy}
        onBack={wizard.goBack}
        onTest={activation.test}
        onActivate={activation.activate}
        {onAddAnother}
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
    position: relative;
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
    color: var(--foreground-hover);
  }
  .steps li.current::before {
    content: '';
    width: 6px;
    height: 6px;
    flex: none;
    border-radius: 50%;
    background: var(--signal);
    box-shadow: 0 0 0 3px var(--signal-soft);
  }
  .steps li.current::after,
  .steps li.complete::after {
    content: '';
    position: absolute;
    right: 0;
    bottom: -1px;
    left: 0;
    height: 1px;
    background: var(--signal);
    transform-origin: left;
    animation: step-rule 520ms var(--ease-out) backwards;
  }
  .steps li.complete::after {
    background: color-mix(in srgb, var(--metric) 55%, transparent);
    animation: none;
  }
  .steps li.complete {
    color: var(--metric);
  }
  @keyframes step-rule {
    from {
      transform: scaleX(0);
    }
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
    .steps li.current::after,
    .steps li.complete::after {
      background: CanvasText;
    }
  }
</style>
