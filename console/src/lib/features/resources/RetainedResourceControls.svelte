<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    requestRetainedReconciliation,
    submitRetainedEvidence,
    type ProviderResource
  } from './api';
  import { draftRetainedEvidence } from './lifecycle';
  import {
    evidenceServerFields,
    reconciliationServerFields,
    validateEvidence,
    type RetainedField
  } from './retainedDraft';

  let {
    resource,
    changed
  }: { resource: ProviderResource; changed: () => void } = $props();
  const access = useRole();
  const canReconcile = $derived(access.can('providers.manage'));
  const canEvidence = $derived(access.can('settings.update'));
  let hint = $state('');
  let evidence = $state('');
  const controlAction = useAction<RetainedField>();
  let section = $state<HTMLElement>();
  const busy = $derived(controlAction.busy);

  usePrincipalLifetime(() => {
    evidence = '';
    hint = '';
  });

  function draftEvidence() {
    evidence = draftRetainedEvidence(resource);
    controlAction.reset();
  }

  async function focusProblem() {
    if (section) await focusFormError(section);
  }

  async function reconcile() {
    if (busy || !canReconcile) return;
    const selectedId = resource.id;
    const scheduled = await controlAction.run(
      () => requestRetainedReconciliation(selectedId, hint),
      () => {
        if (resource.id !== selectedId) return;
        notify(
          'Reconciliation scheduled. Refresh to inspect settlement progress.'
        );
        changed();
      },
      {
        key: 'reconcile',
        failure: (error) =>
          resource.id === selectedId
            ? errorMessage(error, 'Reconciliation could not be scheduled.')
            : '',
        serverFields: reconciliationServerFields
      }
    );
    if (!scheduled) await focusProblem();
  }

  async function submitEvidence(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canEvidence) return;
    const checked = validateEvidence(evidence);
    if (!controlAction.check(checked)) return focusProblem();
    const selectedId = resource.id;
    const recorded = await controlAction.run(
      () => submitRetainedEvidence(selectedId, checked.value),
      (result) => {
        if (resource.id !== selectedId) return;
        notify(
          `Usage evidence recorded. Settlement ${result.settlement_status ?? 'pending'}.`
        );
        changed();
      },
      {
        key: 'evidence',
        failure: (error) =>
          resource.id === selectedId
            ? errorMessage(error, 'Usage evidence could not be recorded.')
            : '',
        serverFields: evidenceServerFields
      }
    );
    if (!recorded) await focusProblem();
  }
</script>

<section
  bind:this={section}
  class="card retained-controls"
  aria-label="Retained resource settlement controls"
>
  <h2>Settlement · <code>{resource.id}</code></h2>
  <p>
    State: {resource.retained_settlement?.state ?? 'unavailable'}. Expiry: {resource.expires_at ??
      'provider managed'}.
  </p>
  {#if resource.retained_settlement?.last_error}<p>
      Missing evidence: {resource.retained_settlement.last_error}
    </p>{/if}
  {#if canReconcile}
    <FormField
      label="Upstream resource hint (optional)"
      error={controlAction.fieldErrors.hint}
    >
      {#snippet control(attributes)}
        <input
          {...attributes}
          bind:value={hint}
          maxlength="512"
          disabled={busy}
        />
      {/snippet}
    </FormField>
    <p>
      A hint is verified against the original submission before a resource can
      be adopted.
    </p>
    <button
      class="button button-secondary"
      type="button"
      onclick={reconcile}
      disabled={busy}
    >
      Reconcile
    </button>
  {/if}
  {#if canEvidence}
    <form onsubmit={submitEvidence}>
      <button
        class="button button-secondary"
        type="button"
        onclick={draftEvidence}
        disabled={busy}
      >
        Prepare usage evidence
      </button>
      <FormField
        label="Verified usage evidence"
        error={controlAction.fieldErrors.evidence}
      >
        {#snippet control(attributes)}
          <textarea
            {...attributes}
            bind:value={evidence}
            rows="15"
            spellcheck="false"
            disabled={busy}
            required></textarea>
        {/snippet}
      </FormField>
      <p>
        Use UTC interval boundaries and an evidence SHA-256 digest. Enter
        verified quantities only; omitted quantities remain unknown. Terminal
        evidence closes liability only after verified remote termination and
        complete coverage.
      </p>
      <button
        class="button button-primary"
        type="submit"
        disabled={busy || !evidence.trim()}
      >
        Record evidence
      </button>
    </form>
  {/if}
  {#if controlAction.error}<p
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
    >
      {controlAction.error}
    </p>{/if}
</section>

<style>
  .retained-controls {
    margin: 1rem 0;
    padding: 1.25rem;
  }
  section :global(.form-field) {
    margin: 0.75rem 0;
  }
  textarea {
    width: 100%;
    font-family: var(--font-mono);
  }
  form {
    margin-top: 1rem;
  }
  p {
    overflow-wrap: anywhere;
  }
</style>
