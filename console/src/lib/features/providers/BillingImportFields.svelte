<script lang="ts">
  import type { BillingImportFormValues } from './billingImportForm.js';

  let {
    form,
    kind,
    azure,
    editing,
    savedEnabled,
    scopeLabel,
    busy,
    onSubmit,
    onRunNow,
    onRemove
  }: {
    form: BillingImportFormValues;
    kind: string;
    azure: boolean;
    editing: boolean;
    savedEnabled: boolean;
    scopeLabel: string;
    busy: boolean;
    onSubmit: (event: SubmitEvent) => void;
    onRunNow: () => void;
    onRemove: () => void;
  } = $props();
</script>

<form class="create-form" onsubmit={onSubmit}>
  <fieldset class="submission-fields" disabled={busy}>
    <div class="form-grid">
      {#if kind === 'bedrock'}
        <div class="form-field">
          <label for="aws-payer">Payer account ID</label><input
            id="aws-payer"
            bind:value={form.payerAccount}
          />
        </div>
        <div class="form-field">
          <label for="aws-bucket">S3 bucket</label><input
            id="aws-bucket"
            bind:value={form.bucket}
          />
        </div>
        <div class="form-field">
          <label for="aws-prefix">CUR report prefix</label><input
            id="aws-prefix"
            bind:value={form.reportPrefix}
            placeholder="prefix/report-name"
          />
        </div>
        <div class="form-field">
          <label for="aws-region">S3 region</label><input
            id="aws-region"
            bind:value={form.region}
            placeholder="us-east-1"
          />
        </div>
        <div class="form-field">
          <label for="aws-linked">Linked account IDs</label><textarea
            id="aws-linked"
            rows="3"
            bind:value={form.linkedAccounts}></textarea><small>
            One 12-digit ID per line, leave empty for all linked accounts
          </small>
        </div>
        {#if editing}<div class="form-field">
            <label class="inline-check">
              <input
                type="checkbox"
                bind:checked={form.replaceCredential}
              />Replace billing credential
            </label>
          </div>{/if}
        {#if !editing || form.replaceCredential}
          <div class="form-field">
            <label for="aws-auth">Authentication</label><select
              id="aws-auth"
              bind:value={form.awsAuthMode}
            >
              <option value="static">Access keys</option><option
                value="workload"
              >
                Workload identity
              </option>
            </select>
          </div>
          {#if form.awsAuthMode === 'static'}
            <div class="form-field">
              <label for="aws-access">Access key ID</label><input
                id="aws-access"
                type="password"
                autocomplete="off"
                bind:value={form.accessKeyId}
              />
            </div>
            <div class="form-field">
              <label for="aws-secret">Secret access key</label><input
                id="aws-secret"
                type="password"
                autocomplete="off"
                bind:value={form.secretAccessKey}
              />
            </div>
            <div class="form-field">
              <label for="aws-token">Session token (optional)</label><input
                id="aws-token"
                type="password"
                autocomplete="off"
                bind:value={form.sessionToken}
              />
            </div>
          {/if}
        {/if}
        <p class="section-help">
          Use a legacy CSV CUR export with create-new-versions enabled and grant
          list and read access to this report
        </p>
      {:else if azure}
        <div class="form-field">
          <label for="billing-azure-scope">
            Billing scope resource ID
          </label><input
            id="billing-azure-scope"
            bind:value={form.billingScope}
          />
        </div>
        <div class="form-field">
          <label for="billing-azure-export">Export name</label><input
            id="billing-azure-export"
            bind:value={form.exportName}
          />
        </div>
        <div class="form-field">
          <label for="billing-azure-account">Storage account</label><input
            id="billing-azure-account"
            bind:value={form.storageAccount}
          />
        </div>
        <div class="form-field">
          <label for="billing-azure-container">Container</label><input
            id="billing-azure-container"
            bind:value={form.container}
          />
        </div>
        <div class="form-field">
          <label for="billing-azure-prefix">
            Storage directory prefix
          </label><input id="billing-azure-prefix" bind:value={form.prefix} />
        </div>
        <div class="form-field">
          <label for="billing-azure-subscriptions">
            Subscription IDs
          </label><textarea
            id="billing-azure-subscriptions"
            rows="3"
            bind:value={form.scopeText}></textarea><small>
            One ID per line. Empty includes all subscriptions in the export.
          </small>
        </div>
        <div class="form-field">
          <label for="billing-azure-auth">Azure credential</label>
          <select id="billing-azure-auth" bind:value={form.authMode}>
            <option value="">
              {editing ? 'Keep stored credential' : 'Choose a credential'}
            </option>
            <option value="azure_default">Workload identity</option>
            <option value="azure_client_secret">Client secret</option>
          </select>
        </div>
        {#if form.authMode === 'azure_client_secret'}
          <div class="form-field">
            <label for="billing-azure-tenant">Tenant ID</label><input
              id="billing-azure-tenant"
              bind:value={form.tenantId}
            />
          </div>
          <div class="form-field">
            <label for="billing-azure-client">Client ID</label><input
              id="billing-azure-client"
              bind:value={form.clientId}
            />
          </div>
          <div class="form-field">
            <label for="billing-azure-secret">Client secret</label><input
              id="billing-azure-secret"
              type="password"
              autocomplete="off"
              bind:value={form.clientSecret}
            />
          </div>
        {/if}
      {:else}
        <div class="form-field">
          {#if kind === 'vertex_ai'}
            <label for="billing-auth-mode">Billing authentication</label>
            <select
              id="billing-auth-mode"
              bind:value={form.googleAuthMode}
              onchange={() => {
                form.adminKey = '';
              }}
            >
              {#if editing}<option value="keep">
                  Keep existing credential
                </option>{/if}
              <option value="service_account">Service account key</option>
              <option value="adc">Workload identity</option>
            </select>
          {/if}
          {#if kind !== 'vertex_ai' || form.googleAuthMode === 'service_account'}
            <label for="billing-admin-key">
              {kind === 'vertex_ai' ? 'Service account JSON' : 'Admin key'}
            </label>
            <input
              id="billing-admin-key"
              type="password"
              autocomplete="off"
              bind:value={form.adminKey}
              placeholder={kind === 'vertex_ai'
                ? 'Paste the service account JSON'
                : editing
                  ? 'Blank keeps the stored key'
                  : 'sk-admin-…'}
            />
          {/if}
        </div>
        {#if kind === 'vertex_ai'}
          <div class="form-field">
            <label for="billing-account">Billing account ID</label><input
              id="billing-account"
              bind:value={form.billingAccountID}
            />
          </div>
          <div class="form-field">
            <label for="billing-project">BigQuery project ID</label><input
              id="billing-project"
              bind:value={form.bigQueryProjectID}
            />
          </div>
          <div class="form-field">
            <label for="billing-dataset">Dataset ID</label><input
              id="billing-dataset"
              bind:value={form.datasetID}
            />
          </div>
          <div class="form-field">
            <label for="billing-table">Detailed export table ID</label><input
              id="billing-table"
              bind:value={form.tableID}
            />
          </div>
        {/if}
        <div class="form-field">
          <label for="billing-scope">{scopeLabel}</label>
          <textarea
            id="billing-scope"
            rows="3"
            bind:value={form.scopeText}
            placeholder={kind === 'vertex_ai'
              ? 'workload-project'
              : kind === 'openai'
                ? 'proj_…'
                : 'wrkspc_… or default'}></textarea>
          <small>
            One ID per line. Leave empty to include all projects or workspaces.
          </small>
        </div>
      {/if}
      <div class="form-field">
        <label class="inline-check">
          <input type="checkbox" bind:checked={form.enabled} />Enabled
        </label>
      </div>
    </div>
    <div class="form-actions">
      <button class="button button-primary" type="submit" disabled={busy}>
        {busy ? 'Saving…' : editing ? 'Save import' : 'Create import'}
      </button>
      {#if editing}
        <button
          class="button button-secondary"
          type="button"
          disabled={busy || !savedEnabled}
          onclick={onRunNow}
        >
          Run now
        </button>
        <button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={onRemove}
        >
          Remove
        </button>
      {/if}
    </div>
    {#if editing && !savedEnabled}
      <p class="section-help">
        Enable and save the billing import to run it manually.
      </p>
    {/if}
  </fieldset>
</form>
