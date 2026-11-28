<script lang="ts">
  import { notify } from '#lib/components/toasts.svelte.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import { resolve } from '$app/paths';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import { formatDate } from '#lib/format.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import BillingImportFields from './BillingImportFields.svelte';

  import {
    deleteBillingImport,
    getBillingImport,
    putBillingImport,
    runBillingImport
  } from '#lib/features/providers/billing.js';
  import {
    billingImportServerFields,
    clearBillingImportSecrets,
    errorPhrase,
    hydrateBillingImport,
    newBillingImportForm,
    validateBillingImport,
    type BillingField
  } from './billingImportForm.js';
  import { providerKeys } from '#lib/domain/providers/providerKeys.js';
  import type { Provider } from '#lib/domain/providers/api.js';

  let { provider, canManage }: { provider: Provider; canManage: boolean } =
    $props();

  const kind = $derived(provider.configuration.kind);
  const azure = $derived(kind === 'azure_openai');
  const supported = $derived(
    kind === 'openai' ||
      kind === 'anthropic' ||
      kind === 'vertex_ai' ||
      kind === 'bedrock' ||
      azure
  );
  const scopeLabel = $derived(
    kind === 'vertex_ai'
      ? 'Google project IDs'
      : kind === 'openai'
        ? 'OpenAI project IDs'
        : 'Anthropic workspace IDs'
  );
  const scopePattern = $derived(
    kind === 'vertex_ai'
      ? /^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$/
      : kind === 'openai'
        ? /^proj_[A-Za-z0-9]{1,64}$/
        : /^wrkspc_[A-Za-z0-9]{1,64}$/
  );

  const saveMutation = useMutation<BillingField>();
  const runMutation = useMutation();
  const removal = useAction();
  const queryClient = useQueryClient();
  const billing = createQuery(() => ({
    queryKey: providerKeys.billingImport(provider.id),
    queryFn: ({ signal }) => getBillingImport(provider.id, signal),
    enabled: supported
  }));

  const form = $state(newBillingImportForm());
  let section = $state<HTMLElement>();
  let reloading = $state(false);
  let reloadError = $state('');
  let initialized = $state(false);

  const captureOwner = usePrincipalLifetime(() => {
    clearBillingImportSecrets(form);
    reloading = false;
    reloadError = '';
    initialized = false;
  });

  const busy = $derived(
    saveMutation.busy || runMutation.busy || removal.busy || reloading
  );
  const error = $derived(
    saveMutation.error || runMutation.error || removal.error || reloadError
  );
  const conflicted = $derived(saveMutation.conflict || removal.conflict);

  const detail = $derived(billing.data ?? null);
  const editing = $derived(detail !== null);

  $effect(() => {
    const current = detail;
    if (current && !initialized) {
      hydrateBillingImport(current, form);
      initialized = true;
    }
  });

  async function focusProblem() {
    if (section) await focusFormError(section);
  }

  async function save() {
    if (!canManage || busy) return;
    const checked = validateBillingImport(form, kind, editing, scopePattern);
    if (!saveMutation.check(checked)) return focusProblem();
    const etag = detail?.etag ?? provider.etag;
    const accepted = await saveMutation.submit(
      checked.value,
      (submission, requestId) =>
        putBillingImport(provider.id, etag, submission, requestId),
      async () => {
        form.adminKey = '';
        form.authMode = '';
        form.tenantId = form.clientId = form.clientSecret = '';
        if (kind === 'vertex_ai') form.googleAuthMode = 'keep';
        form.accessKeyId = form.secretAccessKey = form.sessionToken = '';
        form.replaceCredential = false;
        notify('Billing import saved.');
        await queryClient.invalidateQueries({
          queryKey: providerKeys.billingImport(provider.id)
        });
      },
      {
        failure: (cause) =>
          isEtagMismatch(cause)
            ? ''
            : errorMessage(cause, 'The billing import could not be saved.'),
        serverFields: billingImportServerFields(form, kind)
      }
    );
    if (!accepted) await focusProblem();
  }

  async function reload() {
    if (busy) return;
    const isCurrent = captureOwner();
    reloading = true;
    reloadError = '';
    const result = await readResult(() => billing.refetch());
    if (!isCurrent()) return;
    reloading = false;
    if (!result.ok) {
      reloadError = errorMessage(
        result.error,
        'The billing import could not be reloaded.'
      );
      return;
    }
    if (result.value.error) {
      reloadError = errorMessage(
        result.value.error,
        'The billing import could not be reloaded.'
      );
      return;
    }
    hydrateBillingImport(result.value.data ?? null, form);
    form.adminKey = '';
    form.awsAuthMode = 'static';
    form.accessKeyId = form.secretAccessKey = form.sessionToken = '';
    form.replaceCredential = false;
    form.authMode = '';
    form.tenantId = form.clientId = form.clientSecret = '';
    saveMutation.reset();
    removal.reset();
  }

  async function runNow() {
    if (!canManage || busy || !detail?.enabled) return;
    await runMutation.submit(
      provider.id,
      (id, requestId) => runBillingImport(id, requestId),
      async () => {
        notify('Import scheduled.');
        await queryClient.invalidateQueries({
          queryKey: providerKeys.billingImport(provider.id)
        });
      },
      {
        failure: (cause) =>
          errorMessage(cause, 'The import could not be scheduled.')
      }
    );
  }

  async function remove() {
    if (!canManage || busy || !detail) return;
    if (
      !(await confirmAction({
        title: 'Delete the billing import?',
        body: 'Costs already imported are kept.',
        confirmLabel: 'Delete',
        danger: true
      }))
    )
      return;
    const etag = detail.etag;
    await removal.run(
      () => deleteBillingImport(provider.id, etag),
      async () => {
        form.adminKey = '';
        form.scopeText = '';
        form.billingScope =
          form.exportName =
          form.storageAccount =
          form.container =
          form.prefix =
            '';
        form.authMode = '';
        form.tenantId = form.clientId = form.clientSecret = '';
        form.billingAccountID =
          form.bigQueryProjectID =
          form.datasetID =
          form.tableID =
            '';
        form.googleAuthMode = 'service_account';
        form.enabled = true;
        notify('Billing import removed.');
        await queryClient.invalidateQueries({
          queryKey: providerKeys.billingImport(provider.id)
        });
      },
      {
        failure: (cause) =>
          isEtagMismatch(cause)
            ? ''
            : errorMessage(cause, 'The billing import could not be removed.')
      }
    );
  }
</script>

{#if supported}
  <section
    bind:this={section}
    class="card billing-import"
    aria-labelledby="billing-import-heading"
  >
    <h3 id="billing-import-heading">Billing import</h3>
    <p class="section-help">
      Pull invoiced costs to reconcile attributed usage against billed amounts.
      Billing credentials are separate from inference credentials.
    </p>
    {#if error}<div
        class="inline-problem"
        role="alert"
        tabindex="-1"
        data-error-summary
      >
        {error}
      </div>{/if}

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
          {#if detail.status.source_revision}
            <dt>Source revision</dt>
            <dd class="mono">{detail.status.source_revision}</dd>
          {/if}
          <dt>Next import</dt>
          <dd>{formatDate(detail.status.next_import_at)}</dd>
        </dl>
      {:else if !canManage}
        <p class="section-help">No billing import is configured.</p>
      {/if}

      {#if canManage}
        <BillingImportFields
          {form}
          errors={saveMutation.fieldErrors}
          {kind}
          {azure}
          {editing}
          savedEnabled={detail?.enabled ?? false}
          {scopeLabel}
          {busy}
          onSubmit={(event) => {
            event.preventDefault();
            save();
          }}
          onRunNow={runNow}
          onRemove={remove}
        />
      {:else}
        <ReadOnlyNote>
          Billing imports are managed by connection managers.
        </ReadOnlyNote>
      {/if}

      <p class="section-help">
        <a href={resolve('usage')}>Reconcile billed costs</a>
        against attributed usage on the Usage page.
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
