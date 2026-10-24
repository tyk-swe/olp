<script lang="ts">
  import { apiClient } from '$lib/api/client';
  import type { components } from '$lib/api/schema';
  import { errorMessage, unwrap } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { usePrincipalLifetime } from '$lib/forms/mutationIntent.svelte';
  import {
    nativeObject,
    parseNativeJSON,
    stringifyNativeJSON
  } from '$lib/json/nativeJson';
  import type { ProviderResource } from './api';

  let {
    resource,
    changed
  }: { resource: ProviderResource; changed: () => void } = $props();
  const access = useRole();
  const canReconcile = $derived(access.can('providers.manage'));
  const canEvidence = $derived(access.can('settings.update'));
  let hint = $state('');
  let evidence = $state('');
  let problem = $state('');
  let notice = $state('');
  let busy = $state(false);
  const captureOwner = usePrincipalLifetime(() => {
    evidence = '';
    hint = '';
    problem = '';
    notice = '';
    busy = false;
  });

  function draftEvidence() {
    const quantity =
      resource.kind === 'fine_tuning_job' ||
      resource.kind === 'vertex_tuning_job'
        ? { training_tokens: 0 }
        : resource.kind === 'context_cache'
          ? { cache_token_seconds: '0' }
          : resource.kind === 'container'
            ? { container_sessions: { '1g': 0 } }
            : resource.kind === 'eval_run'
              ? {
                  input_tokens: 0,
                  output_tokens: 0,
                  cached_input_tokens: 0,
                  invocation_count: 0
                }
              : { storage_byte_seconds: '0' };
    evidence = JSON.stringify(
      {
        idempotency_key: crypto.randomUUID(),
        evidence_digest: '',
        start: resource.created_at,
        end: new Date().toISOString(),
        model:
          resource.retained_settlement?.models?.[0] ?? resource.upstream_model,
        ...quantity,
        terminal: false
      },
      null,
      2
    );
    problem = '';
    notice = '';
  }

  async function reconcile() {
    if (busy || !canReconcile) return;
    const currentOwner = captureOwner();
    const selectedId = resource.id;
    busy = true;
    problem = '';
    notice = '';
    try {
      unwrap(
        await apiClient.POST('/api/v1/provider-resources/{id}/reconcile', {
          params: { path: { id: selectedId } },
          body: hint.trim() ? { upstream_id: hint.trim() } : {}
        })
      );
      if (!currentOwner() || resource.id !== selectedId) return;
      notice =
        'Reconciliation scheduled. Refresh to inspect settlement progress.';
      changed();
    } catch (error) {
      if (currentOwner() && resource.id === selectedId)
        problem = errorMessage(error, 'Reconciliation could not be scheduled.');
    } finally {
      if (currentOwner() && resource.id === selectedId) busy = false;
    }
  }

  async function submitEvidence(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canEvidence) return;
    const currentOwner = captureOwner();
    const selectedId = resource.id;
    busy = true;
    problem = '';
    notice = '';
    try {
      const value = parseNativeJSON(evidence);
      if (!nativeObject(value))
        throw new Error('Enter one usage evidence object.');
      // Keep int64 quantities exact through the management transport.
      const serialized = stringifyNativeJSON(value);
      const result = unwrap(
        await apiClient.POST('/api/v1/provider-resources/{id}/usage-evidence', {
          params: { path: { id: selectedId } },
          body: {} as components['schemas']['RetainedUsageEvidenceRequest'],
          bodySerializer: () => serialized
        })
      );
      if (!currentOwner() || resource.id !== selectedId) return;
      notice = `Usage evidence recorded. Settlement ${result.settlement_status ?? 'pending'}.`;
      changed();
    } catch (error) {
      if (currentOwner() && resource.id === selectedId)
        problem = errorMessage(error, 'Usage evidence could not be recorded.');
    } finally {
      if (currentOwner() && resource.id === selectedId) busy = false;
    }
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
    <label
      >Upstream resource hint (optional)<input
        bind:value={hint}
        maxlength="512"
        disabled={busy}
      /></label
    >
    <p>
      A hint is verified against the original submission before a resource can
      be adopted.
    </p>
    <button
      class="button button-secondary"
      type="button"
      onclick={reconcile}
      disabled={busy}>Reconcile</button
    >
  {/if}
  {#if canEvidence}
    <form onsubmit={submitEvidence}>
      <button
        class="button button-secondary"
        type="button"
        onclick={draftEvidence}
        disabled={busy}>Prepare usage evidence</button
      >
      <label
        >Verified usage evidence<textarea
          bind:value={evidence}
          rows="15"
          spellcheck="false"
          disabled={busy}
          required></textarea></label
      >
      <p>
        Use UTC interval boundaries and an evidence SHA-256 digest. Enter
        verified quantities only; omitted quantities remain unknown. Terminal
        evidence closes liability only after verified remote termination and
        complete coverage.
      </p>
      <button
        class="button button-primary"
        type="submit"
        disabled={busy || !evidence.trim()}>Record evidence</button
      >
    </form>
  {/if}
  {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
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
