<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type {
    BillingField,
    BillingImportFormValues
  } from './billingImportForm.js';

  let {
    form,
    errors,
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
    errors: FieldErrors<BillingField>;
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
  const awsAuthModeLabels: Record<
    BillingImportFormValues['awsAuthMode'],
    string
  > = { static: 'Access keys', workload: 'Workload identity' };
  const azureAuthModeLabels: Record<
    Exclude<BillingImportFormValues['authMode'], ''>,
    string
  > = {
    azure_default: 'Workload identity',
    azure_client_secret: 'Client secret'
  };
  const googleAuthModeLabels: Record<
    Exclude<BillingImportFormValues['googleAuthMode'], 'keep'>,
    string
  > = { service_account: 'Service account key', adc: 'Workload identity' };
</script>

<form class="create-form" onsubmit={onSubmit}>
  <fieldset class="submission-fields" disabled={busy}>
    <div class="form-grid">
      {#if kind === 'bedrock'}
        <FormField label="Payer account ID" error={errors.payerAccount}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.payerAccount} />
          {/snippet}
        </FormField>
        <FormField label="S3 bucket" error={errors.bucket}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.bucket} />
          {/snippet}
        </FormField>
        <FormField label="CUR report prefix" error={errors.reportPrefix}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              placeholder="prefix/report-name"
              bind:value={form.reportPrefix}
            />
          {/snippet}
        </FormField>
        <FormField label="S3 region" error={errors.region}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              placeholder="us-east-1"
              bind:value={form.region}
            />
          {/snippet}
        </FormField>
        <FormField label="Linked account IDs" error={errors.linkedAccounts}>
          {#snippet control(attributes)}
            <textarea {...attributes} rows="3" bind:value={form.linkedAccounts}
            ></textarea><small>
              One 12-digit ID per line, leave empty for all linked accounts
            </small>
          {/snippet}
        </FormField>
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
              {#each Object.entries(awsAuthModeLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          </div>
          {#if form.awsAuthMode === 'static'}
            <FormField label="Access key ID" error={errors.accessKeyId}>
              {#snippet control(attributes)}
                <input
                  {...attributes}
                  type="password"
                  autocomplete="off"
                  bind:value={form.accessKeyId}
                />
              {/snippet}
            </FormField>
            <FormField label="Secret access key" error={errors.secretAccessKey}>
              {#snippet control(attributes)}
                <input
                  {...attributes}
                  type="password"
                  autocomplete="off"
                  bind:value={form.secretAccessKey}
                />
              {/snippet}
            </FormField>
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
        <FormField
          label="Billing scope resource ID"
          error={errors.billingScope}
        >
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.billingScope} />
          {/snippet}
        </FormField>
        <FormField label="Export name" error={errors.exportName}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.exportName} />
          {/snippet}
        </FormField>
        <FormField label="Storage account" error={errors.storageAccount}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.storageAccount} />
          {/snippet}
        </FormField>
        <FormField label="Container" error={errors.container}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={form.container} />
          {/snippet}
        </FormField>
        <div class="form-field">
          <label for="billing-azure-prefix">
            Storage directory prefix
          </label><input id="billing-azure-prefix" bind:value={form.prefix} />
        </div>
        <FormField label="Subscription IDs" error={errors.scopeText}>
          {#snippet control(attributes)}
            <textarea {...attributes} rows="3" bind:value={form.scopeText}
            ></textarea><small>
              One ID per line. Empty includes all subscriptions in the export.
            </small>
          {/snippet}
        </FormField>
        <FormField label="Azure credential" error={errors.authMode}>
          {#snippet control(attributes)}
            <select {...attributes} bind:value={form.authMode}>
              <option value="">
                {editing ? 'Keep stored credential' : 'Choose a credential'}
              </option>
              {#each Object.entries(azureAuthModeLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          {/snippet}
        </FormField>
        {#if form.authMode === 'azure_client_secret'}
          <FormField label="Tenant ID" error={errors.tenantId}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.tenantId} />
            {/snippet}
          </FormField>
          <FormField label="Client ID" error={errors.clientId}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.clientId} />
            {/snippet}
          </FormField>
          <FormField label="Client secret" error={errors.clientSecret}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                type="password"
                autocomplete="off"
                bind:value={form.clientSecret}
              />
            {/snippet}
          </FormField>
        {/if}
      {:else}
        {#if kind === 'vertex_ai'}
          <div class="form-field">
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
              {#each Object.entries(googleAuthModeLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          </div>
        {/if}
        {#if kind !== 'vertex_ai' || form.googleAuthMode === 'service_account'}
          <FormField
            label={kind === 'vertex_ai' ? 'Service account JSON' : 'Admin key'}
            error={errors.adminKey}
          >
            {#snippet control(attributes)}
              <input
                {...attributes}
                type="password"
                autocomplete="off"
                bind:value={form.adminKey}
                placeholder={kind === 'vertex_ai'
                  ? 'Paste the service account JSON'
                  : editing
                    ? 'Blank keeps the stored key'
                    : 'sk-admin-…'}
              />
            {/snippet}
          </FormField>
        {/if}
        {#if kind === 'vertex_ai'}
          <FormField label="Billing account ID" error={errors.billingAccountID}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.billingAccountID} />
            {/snippet}
          </FormField>
          <FormField
            label="BigQuery project ID"
            error={errors.bigQueryProjectID}
          >
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.bigQueryProjectID} />
            {/snippet}
          </FormField>
          <FormField label="Dataset ID" error={errors.datasetID}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.datasetID} />
            {/snippet}
          </FormField>
          <FormField label="Detailed export table ID" error={errors.tableID}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={form.tableID} />
            {/snippet}
          </FormField>
        {/if}
        <FormField label={scopeLabel} error={errors.scopeText}>
          {#snippet control(attributes)}
            <textarea
              {...attributes}
              rows="3"
              bind:value={form.scopeText}
              placeholder={kind === 'vertex_ai'
                ? 'workload-project'
                : kind === 'openai'
                  ? 'proj_…'
                  : 'wrkspc_… or default'}></textarea>
            <small>
              One ID per line. Leave empty to include all projects or
              workspaces.
            </small>
          {/snippet}
        </FormField>
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
