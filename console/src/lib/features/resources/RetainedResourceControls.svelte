<script lang="ts">
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    requestRetainedReconciliation,
    submitRetainedEvidence,
    type ProviderResource
  } from './api';
  import { draftRetainedEvidence, parseRetainedEvidence } from './lifecycle';

  let {
    resource,
    changed
  }: { resource: ProviderResource; changed: () => void } = $props();
  const access = useRole();
  const canReconcile = $derived(access.can('providers.manage'));
  const canEvidence = $derived(access.can('settings.update'));
  let hint = $state('');
  let evidence = $state('');
  const controlAction = useAction();
  const busy = $derived(controlAction.busy);

  usePrincipalLifetime(() => {
    evidence = '';
    hint = '';
  });

  function draftEvidence() {
    evidence = draftRetainedEvidence(resource);
    controlAction.reset();
  }

  async function reconcile() {
    if (busy || !canReconcile) return;
    const selectedId = resource.id;
    await controlAction.run(
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
            : ''
      }
    );
  }

  async function submitEvidence(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canEvidence) return;
    const parsed = parseRetainedEvidence(evidence);
    if (!parsed.ok) {
      controlAction.error = parsed.message;
      return;
    }
    const selectedId = resource.id;
    await controlAction.run(
      () => submitRetainedEvidence(selectedId, parsed.value),
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
            : ''
      }
    );
  }
</script>

<section
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
    <label>
      Upstream resource hint (optional)<input
        bind:value={hint}
        maxlength="512"
        disabled={busy}
      />
    </label>
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
      <label>
        Verified usage evidence<textarea
          bind:value={evidence}
          rows="15"
          spellcheck="false"
          disabled={busy}
          required></textarea>
      </label>
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
  {#if controlAction.error}<p class="inline-problem" role="alert">
      {controlAction.error}
    </p>{/if}
</section>

<style>
  .retained-controls {
    margin: 1rem 0;
    padding: 1.25rem;
  }
  label {
    display: grid;
    gap: 0.4rem;
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
