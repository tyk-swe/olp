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
    type BillingImport,
    type PutBillingImportInput
  } from '$lib/features/providers/api/billing';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import type { Provider } from '$lib/features/providers/api/providers';

  let { provider, canManage }: { provider: Provider; canManage: boolean } =
    $props();

  const kind = $derived(provider.configuration.kind);
  const azure = $derived(kind === 'azure_openai');
  const supported = $derived(
    kind === 'openai' || kind === 'anthropic' || kind === 'bedrock' || azure
  );
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
  let payerAccount = $state('');
  let bucket = $state('');
  let reportPrefix = $state('');
  let region = $state('');
  let linkedAccounts = $state('');
  let awsAuthMode = $state<'static' | 'workload'>('static');
  let accessKeyId = $state('');
  let secretAccessKey = $state('');
  let sessionToken = $state('');
  let replaceCredential = $state(false);
  let scopeText = $state('');
  let billingScope = $state('');
  let exportName = $state('');
  let storageAccount = $state('');
  let container = $state('');
  let prefix = $state('');
  let authMode = $state<'' | 'azure_default' | 'azure_client_secret'>('');
  let tenantId = $state('');
  let clientId = $state('');
  let clientSecret = $state('');
  let enabled = $state(true);
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let conflicted = $state(false);
  let initialized = $state(false);

  const captureOwner = usePrincipalLifetime(() => {
    adminKey = '';
    payerAccount = bucket = reportPrefix = region = linkedAccounts = '';
    accessKeyId = secretAccessKey = sessionToken = '';
    replaceCredential = false;
    scopeText = '';
    billingScope = exportName = storageAccount = container = prefix = '';
    authMode = '';
    tenantId = clientId = clientSecret = '';
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
      if ('billing_scope' in current.scope) {
        const scope = current.scope;
        billingScope = scope.billing_scope ?? '';
        exportName = scope.export_name ?? '';
        storageAccount = scope.storage_account ?? '';
        container = scope.container ?? '';
        prefix = scope.prefix ?? '';
      } else if ('payer_account_id' in current.scope) {
        const scope = current.scope;
        payerAccount = scope.payer_account_id;
        bucket = scope.bucket;
        reportPrefix = scope.report_prefix;
        region = scope.region;
        linkedAccounts = (scope.linked_account_ids ?? []).join('\n');
      }
      enabled = current.enabled;
      initialized = true;
    }
  });

  function scopeLines(document: BillingImport): string {
    const scope = document.scope;
    if ('payer_account_id' in scope) return '';
    if ('billing_scope' in scope)
      return (scope.subscription_ids ?? []).join('\n');
    const entries = scope.project_ids ?? scope.workspace_ids ?? [];
    return entries.join('\n');
  }

  function errorPhrase(code: string): string {
    switch (code) {
      case 'credential':
        return 'the vendor rejected the billing credential';
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

  function scopeBody(): NonNullable<PutBillingImportInput['scope']> {
    if (kind === 'bedrock') {
      const ids = linkedAccounts
        .split('\n')
        .map((line) => line.trim())
        .filter(Boolean);
      return {
        payer_account_id: payerAccount.trim(),
        bucket: bucket.trim(),
        report_prefix: reportPrefix.trim(),
        region: region.trim(),
        ...(ids.length ? { linked_account_ids: ids } : {})
      };
    }
    const ids = scopeText
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    if (azure) {
      return {
        billing_scope: billingScope.trim(),
        export_name: exportName.trim(),
        storage_account: storageAccount.trim(),
        container: container.trim(),
        ...(prefix.trim() ? { prefix: prefix.trim() } : {}),
        ...(ids.length ? { subscription_ids: ids } : {})
      };
    }
    if (!ids.length) return {};
    return kind === 'openai' ? { project_ids: ids } : { workspace_ids: ids };
  }

  function validateForm(): string {
    if (kind === 'bedrock') {
      if (!/^\d{12}$/.test(payerAccount.trim()))
        return 'Enter the 12-digit payer account ID';
      if (!bucket.trim() || !reportPrefix.trim() || !region.trim())
        return 'Enter the bucket, report prefix, and region';
      const ids = linkedAccounts
        .split('\n')
        .map((line) => line.trim())
        .filter(Boolean);
      if (ids.length > 100 || ids.some((id) => !/^\d{12}$/.test(id)))
        return 'Enter at most 100 12-digit linked account IDs';
      if (
        (!editing || replaceCredential) &&
        awsAuthMode === 'static' &&
        (!accessKeyId.trim() || !secretAccessKey.trim())
      )
        return 'Enter an access key ID and secret access key';
      return '';
    }
    const ids = scopeText
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    if (ids.length > 100) return 'At most 100 scope IDs can be listed.';
    if (azure) {
      if (
        !billingScope.trim() ||
        !exportName.trim() ||
        !storageAccount.trim() ||
        !container.trim()
      )
        return 'Billing scope, export name, storage account, and container are required.';
      if (
        ids.some(
          (id) =>
            !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
              id
            )
        )
      )
        return 'Subscription IDs must be UUIDs.';
      if (!editing && !authMode) return 'Choose an Azure credential mode.';
      if (
        authMode === 'azure_client_secret' &&
        (!tenantId.trim() || !clientId.trim() || !clientSecret)
      )
        return 'Tenant ID, client ID, and client secret are required.';
      return '';
    }
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
    const input: Parameters<typeof putBillingImport>[2] = {
      enabled,
      scope: scopeBody(),
      ...(kind === 'bedrock'
        ? !editing || replaceCredential
          ? {
              credential:
                awsAuthMode === 'workload'
                  ? { auth_mode: 'workload' as const }
                  : {
                      auth_mode: 'static' as const,
                      access_key_id: accessKeyId.trim(),
                      secret_access_key: secretAccessKey.trim(),
                      ...(sessionToken.trim()
                        ? { session_token: sessionToken.trim() }
                        : {})
                    }
            }
          : {}
        : azure && authMode
          ? {
              credential:
                authMode === 'azure_default'
                  ? { auth_mode: authMode }
                  : {
                      auth_mode: authMode,
                      tenant_id: tenantId.trim(),
                      client_id: clientId.trim(),
                      client_secret: clientSecret
                    }
            }
          : adminKey.trim()
            ? { credential: adminKey.trim() }
            : {})
    };
    const isCurrent = captureOwner();
    busy = true;
    error = notice = '';
    conflicted = false;
    try {
      await intent.submit(input, (submission, requestId) =>
        putBillingImport(provider.id, etag, submission, requestId)
      );
      if (!isCurrent()) return;
      adminKey = '';
      authMode = '';
      tenantId = clientId = clientSecret = '';
      accessKeyId = secretAccessKey = sessionToken = '';
      replaceCredential = false;
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
      billingScope = exportName = storageAccount = container = prefix = '';
      authMode = '';
      tenantId = clientId = clientSecret = '';
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
      billed amounts. Billing credentials are separate from inference
      credentials.
    </p>
    {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
    {#if notice}<p class="section-help" role="status">{notice}</p>{/if}
    {#if conflicted}
      <ConflictNotice
        notice="conflict"
        onReload={async () => {
          await billing.refetch();
        }}
        disabled={busy}
      />
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
        <form
          class="create-form"
          onsubmit={(event) => {
            event.preventDefault();
            save();
          }}
        >
          <fieldset class="submission-fields" disabled={busy}>
            <div class="form-grid">
              {#if kind === 'bedrock'}
                <div class="form-field">
                  <label for="aws-payer">Payer account ID</label><input
                    id="aws-payer"
                    bind:value={payerAccount}
                  />
                </div>
                <div class="form-field">
                  <label for="aws-bucket">S3 bucket</label><input
                    id="aws-bucket"
                    bind:value={bucket}
                  />
                </div>
                <div class="form-field">
                  <label for="aws-prefix">CUR report prefix</label><input
                    id="aws-prefix"
                    bind:value={reportPrefix}
                    placeholder="prefix/report-name"
                  />
                </div>
                <div class="form-field">
                  <label for="aws-region">S3 region</label><input
                    id="aws-region"
                    bind:value={region}
                    placeholder="us-east-1"
                  />
                </div>
                <div class="form-field">
                  <label for="aws-linked">Linked account IDs</label><textarea
                    id="aws-linked"
                    rows="3"
                    bind:value={linkedAccounts}></textarea><small
                    >One 12-digit ID per line, leave empty for all linked
                    accounts</small
                  >
                </div>
                {#if editing}<div class="form-field">
                    <label class="inline-check"
                      ><input
                        type="checkbox"
                        bind:checked={replaceCredential}
                      />Replace billing credential</label
                    >
                  </div>{/if}
                {#if !editing || replaceCredential}
                  <div class="form-field">
                    <label for="aws-auth">Authentication</label><select
                      id="aws-auth"
                      bind:value={awsAuthMode}
                      ><option value="static">Access keys</option><option
                        value="workload">Workload identity</option
                      ></select
                    >
                  </div>
                  {#if awsAuthMode === 'static'}
                    <div class="form-field">
                      <label for="aws-access">Access key ID</label><input
                        id="aws-access"
                        type="password"
                        autocomplete="off"
                        bind:value={accessKeyId}
                      />
                    </div>
                    <div class="form-field">
                      <label for="aws-secret">Secret access key</label><input
                        id="aws-secret"
                        type="password"
                        autocomplete="off"
                        bind:value={secretAccessKey}
                      />
                    </div>
                    <div class="form-field">
                      <label for="aws-token">Session token (optional)</label
                      ><input
                        id="aws-token"
                        type="password"
                        autocomplete="off"
                        bind:value={sessionToken}
                      />
                    </div>
                  {/if}
                {/if}
                <p class="section-help">
                  Use a legacy CSV CUR export with create-new-versions enabled
                  and grant list and read access to this report
                </p>
              {:else if azure}
                <div class="form-field">
                  <label for="billing-azure-scope"
                    >Billing scope resource ID</label
                  ><input id="billing-azure-scope" bind:value={billingScope} />
                </div>
                <div class="form-field">
                  <label for="billing-azure-export">Export name</label><input
                    id="billing-azure-export"
                    bind:value={exportName}
                  />
                </div>
                <div class="form-field">
                  <label for="billing-azure-account">Storage account</label
                  ><input
                    id="billing-azure-account"
                    bind:value={storageAccount}
                  />
                </div>
                <div class="form-field">
                  <label for="billing-azure-container">Container</label><input
                    id="billing-azure-container"
                    bind:value={container}
                  />
                </div>
                <div class="form-field">
                  <label for="billing-azure-prefix"
                    >Storage directory prefix</label
                  ><input id="billing-azure-prefix" bind:value={prefix} />
                </div>
                <div class="form-field">
                  <label for="billing-azure-subscriptions"
                    >Subscription IDs</label
                  ><textarea
                    id="billing-azure-subscriptions"
                    rows="3"
                    bind:value={scopeText}></textarea><small
                    >One ID per line. Empty includes all subscriptions in the
                    export.</small
                  >
                </div>
                <div class="form-field">
                  <label for="billing-azure-auth">Azure credential</label>
                  <select id="billing-azure-auth" bind:value={authMode}>
                    <option value=""
                      >{editing
                        ? 'Keep stored credential'
                        : 'Choose a credential'}</option
                    >
                    <option value="azure_default">Workload identity</option>
                    <option value="azure_client_secret">Client secret</option>
                  </select>
                </div>
                {#if authMode === 'azure_client_secret'}
                  <div class="form-field">
                    <label for="billing-azure-tenant">Tenant ID</label><input
                      id="billing-azure-tenant"
                      bind:value={tenantId}
                    />
                  </div>
                  <div class="form-field">
                    <label for="billing-azure-client">Client ID</label><input
                      id="billing-azure-client"
                      bind:value={clientId}
                    />
                  </div>
                  <div class="form-field">
                    <label for="billing-azure-secret">Client secret</label
                    ><input
                      id="billing-azure-secret"
                      type="password"
                      autocomplete="off"
                      bind:value={clientSecret}
                    />
                  </div>
                {/if}
              {:else}
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
              {/if}
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
