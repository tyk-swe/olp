<script lang="ts" generics="Values extends ProviderEditValues">
  import { createQuery } from '@tanstack/svelte-query';
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { ConnectionField } from './wizard/connectionValidation';
  import { listProviderProfiles } from './providerProfiles';
  import { providerKeys } from '#lib/domain/providers/providerKeys.js';
  import type { ProviderKindCapability } from '#lib/domain/providers/api.js';
  import type { components } from '#lib/management/schema.js';
  import { nativeObject, type NativeValue } from '#lib/nativeJson.js';
  import type { ProviderEditValues } from './providerEditor';
  import {
    authOptionsFor,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    showsEndpoint
  } from './providerKindSpec';

  let {
    values = $bindable(),
    spec,
    idPrefix,
    disabled = false,
    authEditable = false,
    endpointReadonly = false,
    errors = {},
    onChange
  }: {
    values: Values;
    spec: ProviderKindCapability;
    idPrefix: string;
    disabled?: boolean;
    authEditable?: boolean;
    endpointReadonly?: boolean;
    errors?: FieldErrors<ConnectionField>;
    onChange?: () => void;
  } = $props();
  const credentialSourceLabels: Record<
    NonNullable<
      components['schemas']['ProviderConfiguration']['credential_source']
    >,
    string
  > = {
    operator: 'Operator-held credentials',
    caller: 'Caller supplies each request'
  };
  const callerAuthModes: readonly string[] = [
    'api_key',
    'headers',
    'static'
  ] satisfies components['schemas']['ProviderAuthMode'][];
  const profiles = createQuery(() => ({
    queryKey: providerKeys.profiles(),
    queryFn: ({ signal }) => listProviderProfiles(signal),
    enabled: Boolean(values.profileId)
  }));
  const profile = $derived(
    profiles.data?.find(
      (profile) =>
        profile.id === values.profileId &&
        profile.revision === values.profileRevision
    )
  );
  const v1 = $derived(profile?.hosting === 'azure-v1');
  const authOptions = $derived(
    authOptionsFor(spec).filter(
      ([mode]) => !profile || profile.authentication.includes(mode)
    )
  );
  const fieldsDisabled = $derived(
    disabled || values.document?.fieldsAvailable === false
  );
  const batchStorageProfile = $derived(
    values.profileId === 'vertex-batch-prediction' ||
      values.profileId === 'bedrock-batch-inference'
  );
  const batchStorageValue = (field: string) =>
    values.document?.text(['batch_storage', field]) ?? '';
  function setBatchStorageValue(field: string, value: string) {
    const current = values.document?.at(['batch_storage']);
    const storage: Record<string, NativeValue> = nativeObject(current)
      ? { ...current }
      : {};
    storage[field] = value;
    values.document?.set(['batch_storage'], storage);
    onChange?.();
  }
  const batchStoragePlaceholder = (field: 'input' | 'output') =>
    values.profileId === 'vertex-batch-prediction'
      ? `gs://bucket/${field}/`
      : `s3://bucket/${field}/`;
  const documentProcessingProfile = $derived(
    values.profileId === 'vertex-document-processing-ocr'
  );
  const documentPrefixes = () => {
    const prefixes = values.document?.at(['options', 'document_uri_prefixes']);
    return Array.isArray(prefixes)
      ? prefixes.filter((value) => typeof value === 'string').join('\n')
      : '';
  };
  function setDocumentPrefixes(value: string) {
    values.document?.set(
      ['options', 'document_uri_prefixes'],
      value
        .split('\n')
        .map((prefix) => prefix.trim())
        .filter(Boolean)
    );
    onChange?.();
  }
</script>

<FormField label="Provider name" error={errors.name}>
  {#snippet control(attributes)}
    <input
      {...attributes}
      bind:value={values.name}
      oninput={onChange}
      disabled={fieldsDisabled}
      required
    />
  {/snippet}
</FormField>
<FormField label="Authentication" error={errors.authMode}>
  {#snippet control(attributes)}
    {#if authEditable || values.profileId}
      <select
        {...attributes}
        bind:value={values.authMode}
        onchange={onChange}
        disabled={fieldsDisabled}
      >
        {#if !authOptions.some(([mode]) => mode === values.authMode)}<option
            value={values.authMode}
            disabled
          >
            Current authentication is incompatible
          </option>{/if}
        {#each authOptions as option (option[0])}
          <option value={option[0]}>{option[1]}</option>
        {/each}
      </select>
    {:else}
      <input {...attributes} value={values.authMode} disabled />
    {/if}
  {/snippet}
</FormField>
<div class="form-field full">
  <label for={`${idPrefix}-credential-source`}>Serving credentials</label>
  <select
    id={`${idPrefix}-credential-source`}
    value={values.document?.text(['credential_source']) || 'operator'}
    disabled={fieldsDisabled}
    onchange={(event) => {
      values.document?.set(['credential_source'], event.currentTarget.value);
      onChange?.();
    }}
  >
    {#each Object.entries(credentialSourceLabels) as [value, label] (value)}
      <option
        {value}
        disabled={value === 'caller' &&
          (spec.kind === 'plugin' ||
            !callerAuthModes.includes(values.authMode))}
      >
        {label}
      </option>
    {/each}
  </select>
  <small>
    Caller mode requires X-OLP-Provider-Credential and keeps the endpoint and
    profile fixed. Keep an operator credential for probes and certification.
    Caller secrets are never saved or shared between targets.
  </small>
</div>
<!-- A plugin provider's endpoint is its profile's address, which the server sets. -->
{#if showsEndpoint(values, spec)}
  <FormField
    label={spec.kind === 'azure_openai'
      ? 'Azure resource endpoint'
      : 'Endpoint'}
    error={errors.endpoint}
    full
  >
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="url"
        bind:value={values.endpoint}
        oninput={onChange}
        readonly={endpointReadonly}
        disabled={fieldsDisabled}
        required={hasCustomEndpoint(spec)}
      />
    {/snippet}
  </FormField>
{/if}
{#if hasApiVersion(spec)}
  <FormField label="API version" error={errors.apiVersion}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        bind:value={values.apiVersion}
        oninput={onChange}
        disabled={fieldsDisabled}
        required={!v1}
        aria-describedby={[
          attributes['aria-describedby'],
          v1 && `${idPrefix}-version-help`
        ]
          .filter(Boolean)
          .join(' ') || undefined}
      />
      {#if v1}<small id={`${idPrefix}-version-help`}>
          Azure v1 does not use a dated API version. Clear this field explicitly
          when migrating.
        </small>{/if}
    {/snippet}
  </FormField>
{/if}
{#if hasCloudRegion(spec)}
  <FormField label="Cloud region" error={errors.cloudRegion}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        bind:value={values.cloudRegion}
        oninput={onChange}
        disabled={fieldsDisabled}
        required
      />
    {/snippet}
  </FormField>
{/if}
{#if hasCloudProject(spec)}
  <FormField label="Cloud project" error={errors.cloudProject}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        bind:value={values.cloudProject}
        oninput={onChange}
        disabled={fieldsDisabled}
        required
      />
    {/snippet}
  </FormField>
{/if}
{#if batchStorageProfile}
  <div class="form-field full">
    <label for={`${idPrefix}-batch-input-uri-prefix`}>
      Batch input URI prefix
    </label>
    <input
      id={`${idPrefix}-batch-input-uri-prefix`}
      value={batchStorageValue('input_uri_prefix')}
      oninput={(event) =>
        setBatchStorageValue('input_uri_prefix', event.currentTarget.value)}
      disabled={fieldsDisabled}
      placeholder={batchStoragePlaceholder('input')}
    />
    <small>Allowed object storage prefix for batch input files.</small>
  </div>
  <div class="form-field full">
    <label for={`${idPrefix}-batch-output-uri-prefix`}>
      Batch output URI prefix
    </label>
    <input
      id={`${idPrefix}-batch-output-uri-prefix`}
      value={batchStorageValue('output_uri_prefix')}
      oninput={(event) =>
        setBatchStorageValue('output_uri_prefix', event.currentTarget.value)}
      disabled={fieldsDisabled}
      placeholder={batchStoragePlaceholder('output')}
    />
    <small>Prefix must be owned by the operator and ends with a slash.</small>
  </div>
  {#if values.profileId === 'bedrock-batch-inference'}
    <div class="form-field full">
      <label for={`${idPrefix}-batch-role-arn`}>Batch job IAM role ARN</label>
      <input
        id={`${idPrefix}-batch-role-arn`}
        value={batchStorageValue('role_arn')}
        oninput={(event) =>
          setBatchStorageValue('role_arn', event.currentTarget.value)}
        disabled={fieldsDisabled}
        placeholder="arn:aws:iam::123456789012:role/BedrockBatch"
      />
    </div>
  {/if}
{/if}
{#if documentProcessingProfile}
  <div class="form-field full">
    <label for={`${idPrefix}-document-uri-prefixes`}>
      Document URI prefixes
    </label>
    <textarea
      id={`${idPrefix}-document-uri-prefixes`}
      value={documentPrefixes()}
      oninput={(event) => setDocumentPrefixes(event.currentTarget.value)}
      disabled={fieldsDisabled}
      placeholder="gs://bucket/documents/"
      rows="3"></textarea>
    <small>
      One GCS prefix per line. Native document locations outside these prefixes
      are refused, and an empty list grants no document locations. Inline
      documents need no prefix. The cloud region is the Document AI location,
      such as us or eu, and each model names a processor ID.
    </small>
  </div>
{/if}
{#if hasDeployment(spec)}
  <FormField label="Cloud deployment" error={errors.deployment}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        bind:value={values.deployment}
        oninput={onChange}
        disabled={fieldsDisabled}
        required={!v1}
      />
    {/snippet}
  </FormField>
{/if}
