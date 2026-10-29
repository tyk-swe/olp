<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '$lib/api/http';
  import { resolve } from '$app/paths';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import { formatDate } from '$lib/format';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';
  import {
    deleteBillingImport,
    getBillingImport,
    putBillingImport,
    runBillingImport,
    type BillingImport
  } from '$lib/features/providers/api/billing';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import type { Provider } from '$lib/features/providers/api/providers';

  let { provider, canManage }: { provider: Provider; canManage: boolean } =
    $props();

  const kind = $derived(provider.configuration.kind);
  const supported = $derived(kind === 'openai' || kind === 'anthropic');
  const scopeLabel = $derived(
    kind === 'openai' ? 'OpenAI project IDs' : 'Anthropic workspace IDs'
  );
  const scopePattern = $derived(
    kind === 'openai'
      ? /^proj_[A-Za-z0-9]{1,64}$/
      : /^wrkspc_[A-Za-z0-9]{1,64}$/
  );

  const intent = useMutationIntent();
  const queryClient = useQueryClient();
  const billing = createQuery(() => ({
    queryKey: providerKeys.billingImport(provider.id),
    queryFn: ({ signal }) => getBillingImport(provider.id, signal),
    enabled: supported
  }));

  let adminKey = $state('');
  let scopeText = $state('');
  let enabled = $state(true);
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let conflicted = $state(false);
  let initialized = $state(false);

  const captureOwner = usePrincipalLifetime(() => {
    adminKey = '';
    scopeText = '';
    enabled = true;
    busy = false;
    error = notice = '';
    conflicted = false;
    initialized = false;
  });

  const detail = $derived(billing.data ?? null);
  const editing = $derived(detail !== null);

  $effect(() => {
    const current = detail;
    if (current && !initialized) {
      scopeText = scopeLines(current);
      enabled = current.enabled;
      initialized = true;
    }
  });

  function scopeLines(document: BillingImport): string {
    const scope = document.scope as Record<string, string[]>;
    const entries = scope.project_ids ?? scope.workspace_ids ?? [];
    return entries.join('\n');
  }

  function errorPhrase(code: string): string {
    switch (code) {
      case 'credential':
        return 'the vendor rejected the admin key';
      case 'status':
        return 'the vendor returned an error';
      case 'network':
        return 'the vendor endpoint was unreachable';
      case 'timeout':
        return 'the vendor did not answer in time';
      case 'response':
        return 'the vendor returned an unexpected response';
      case 'endpoint':
        return 'egress to the endpoint was refused';
      default:
        return code;
    }
  }

  function scopeBody(): Record<string, never> {
    const ids = scopeText
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    if (!ids.length) return {};
    // The contract types scope as an empty-object record; the runtime accepts
    // the vendor's ID lists.
    const scope: Record<string, string[]> =
      kind === 'openai' ? { project_ids: ids } : { workspace_ids: ids };
    return scope as Record<string, never>;
  }

  function validateForm(): string {
    const ids = scopeText
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    if (ids.length > 100) return 'At most 100 scope IDs can be listed.';
    for (const id of ids) {
      if (kind === 'anthropic' && id === 'default') continue;
      if (!scopePattern.test(id))
        return `${id} is not a valid ${kind === 'openai' ? 'project' : 'workspace'} ID.`;
    }
    if (!editing && !adminKey.trim())
      return 'An admin key is required to create the import.';
    return '';
  }

  async function save() {
    if (!canManage || busy) return;
    const invalid = validateForm();
    if (invalid) {
      error = invalid;
      return;
    }
    const etag = detail?.etag ?? provider.etag;
    const submission = intent.prepare<Parameters<typeof putBillingImport>[2]>({
      enabled,
      scope: scopeBody(),
      ...(adminKey.trim() ? { credential: adminKey.trim() } : {})
    });
    const isCurrent = captureOwner();
    busy = true;
    error = notice = '';
    conflicted = false;
    try {
      await putBillingImport(
        provider.id,
        etag,
        submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      adminKey = '';
      notice = 'Billing import saved.';
      await queryClient.invalidateQueries({
        queryKey: providerKeys.billingImport(provider.id)
      });
    } catch (cause) {
      if (!isCurrent()) return;
      if (isEtagMismatch(cause)) {
        conflicted = true;
        return;
      }
      error = errorMessage(cause, 'The billing import could not be saved.');
    } finally {
      if (isCurrent()) busy = false;
    }
  }

  async function reload() {
    if (busy) return;
    const isCurrent = captureOwner();
    busy = true;
    error = notice = '';
    try {
      const result = await billing.refetch();
      if (!isCurrent()) return;
      if (result.error) {
        error = errorMessage(
          result.error,
          'The billing import could not be reloaded.'
        );
        return;
      }
      scopeText = result.data ? scopeLines(result.data) : '';
      enabled = result.data?.enabled ?? true;
      adminKey = '';
      initialized = true;
      conflicted = false;
      intent.reset();
    } catch (cause) {
      if (isCurrent())
        error = errorMessage(
          cause,
          'The billing import could not be reloaded.'
        );
    } finally {
      if (isCurrent()) busy = false;
    }
  }

  async function runNow() {
    if (!canManage || busy) return;
    const isCurrent = captureOwner();
    busy = true;
    error = notice = '';
    try {
      await runBillingImport(provider.id, crypto.randomUUID());
      if (!isCurrent()) return;
      notice = 'Import scheduled.';
      await queryClient.invalidateQueries({
        queryKey: providerKeys.billingImport(provider.id)
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The import could not be scheduled.');
    } finally {
      if (isCurrent()) busy = false;
    }
  }

  async function remove() {
    if (!canManage || busy || !detail) return;
    if (!confirm('Delete the billing import? Imported costs are kept.')) return;
    const isCurrent = captureOwner();
    busy = true;
    error = notice = '';
    try {
      await deleteBillingImport(provider.id, detail.etag);
      if (!isCurrent()) return;
      adminKey = '';
      scopeText = '';
      enabled = true;
      notice = 'Billing import removed.';
      await queryClient.invalidateQueries({
        queryKey: providerKeys.billingImport(provider.id)
      });
    } catch (cause) {
      if (!isCurrent()) return;
      if (isEtagMismatch(cause)) {
        conflicted = true;
        return;
      }
      error = errorMessage(cause, 'The billing import could not be removed.');
    } finally {
      if (isCurrent()) busy = false;
    }
  }
</script>

{#if supported}
  <section class="card billing-import" aria-labelledby="billing-import-heading">
    <h3 id="billing-import-heading">Billing import</h3>
    <p class="section-help">
      Pull the vendor's invoiced costs to reconcile attributed usage against
      billed amounts. The credential must be an admin key — it never serves
      inference.
    </p>
    {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
    {#if notice}<p class="section-help" role="status">{notice}</p>{/if}
    {#if conflicted}
      <ConflictNotice notice="conflict" onReload={reload} disabled={busy} />
    {/if}

    {#if billing.isPending}
      <span class="inline-status" role="status">Loading billing import…</span>
    {:else}
      {#if detail}
        <dl>
          <dt>Enabled</dt>
          <dd>{detail.enabled ? 'Yes' : 'No'}</dd>
          <dt>Source</dt>
          <dd class="mono">{detail.source}</dd>
          <dt>Last success</dt>
          <dd>{formatDate(detail.status.last_success_at)}</dd>
          {#if detail.status.last_error_code}
            <dt>Last error</dt>
            <dd>{errorPhrase(detail.status.last_error_code)}</dd>
          {/if}
          <dt>Imported range</dt>
          <dd>
            {detail.status.imported_from ?? '—'} → {detail.status
              .imported_through ?? '—'}
          </dd>
          <dt>Next import</dt>
          <dd>{formatDate(detail.status.next_import_at)}</dd>
        </dl>
      {:else if !canManage}
        <p class="section-help">No billing import is configured.</p>
      {/if}

      {#if canManage}
        <form
          class="create-form"
          onsubmit={(event) => {
            event.preventDefault();
            save();
          }}
        >
          <fieldset class="submission-fields" disabled={busy}>
            <div class="form-grid">
              <div class="form-field">
                <label for="billing-admin-key">Admin key</label>
                <input
                  id="billing-admin-key"
                  type="password"
                  autocomplete="off"
                  bind:value={adminKey}
                  placeholder={editing
                    ? 'Blank keeps the stored key'
                    : 'sk-admin-…'}
                />
              </div>
              <div class="form-field">
                <label for="billing-scope">{scopeLabel}</label>
                <textarea
                  id="billing-scope"
                  rows="3"
                  bind:value={scopeText}
                  placeholder={kind === 'openai'
                    ? 'proj_…'
                    : 'wrkspc_… or default'}></textarea>
                <small
                  >One ID per line; empty imports the whole organization.</small
                >
              </div>
              <div class="form-field">
                <label class="inline-check"
                  ><input
                    type="checkbox"
                    bind:checked={enabled}
                  />Enabled</label
                >
              </div>
            </div>
            <div class="form-actions">
              <button
                class="button button-primary"
                type="submit"
                disabled={busy}
                >{busy
                  ? 'Saving…'
                  : editing
                    ? 'Save import'
                    : 'Create import'}</button
              >
              {#if editing}
                <button
                  class="button button-secondary"
                  type="button"
                  disabled={busy}
                  onclick={runNow}>Run now</button
                >
                <button
                  class="button button-secondary"
                  type="button"
                  disabled={busy}
                  onclick={remove}>Remove</button
                >
              {/if}
            </div>
          </fieldset>
        </form>
      {:else}
        <ReadOnlyNote
          >Billing imports are managed by connection managers.</ReadOnlyNote
        >
      {/if}

      <p class="section-help">
        <a href={resolve('/usage')}>Reconcile billed costs</a> against attributed
        usage on the Usage page.
      </p>
    {/if}
  </section>
{/if}

<style>
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dt {
    font-weight: 500;
  }
  dd {
    margin: 0;
    color: var(--foreground-subtle);
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
