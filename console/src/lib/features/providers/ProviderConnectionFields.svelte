<script lang="ts" generics="Values extends ProviderEditValues">
  import { createQuery } from '@tanstack/svelte-query';
  import { listProviderProfiles } from './api/profiles';
  import { providerKeys } from './providerKeys';
  import type { ProviderKindCapability } from './api/models';
  import { nativeObject, type NativeValue } from '$lib/json/nativeJson';
  import {
    authOptionsFor,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    type ProviderEditValues
  } from './providerEditor';

  let {
    values = $bindable(),
    spec,
    idPrefix,
    disabled = false,
    authEditable = false,
    endpointReadonly = false,
    onChange
  }: {
    values: Values;
    spec: ProviderKindCapability;
    idPrefix: string;
    disabled?: boolean;
    authEditable?: boolean;
    endpointReadonly?: boolean;
    onChange?: () => void;
  } = $props();
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

<div class="form-field">
  <label for={`${idPrefix}-name`}>Provider name</label>
  <input
    id={`${idPrefix}-name`}
    bind:value={values.name}
    oninput={onChange}
    disabled={fieldsDisabled}
    required
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-auth`}>Authentication</label>
  {#if authEditable || values.profileId}
    <select
      id={`${idPrefix}-auth`}
      bind:value={values.authMode}
      onchange={onChange}
      disabled={fieldsDisabled}
    >
      {#if !authOptions.some(([mode]) => mode === values.authMode)}<option
          value={values.authMode}
          disabled>Current authentication is incompatible</option
        >{/if}
      {#each authOptions as option (option[0])}
        <option value={option[0]}>{option[1]}</option>
      {/each}
    </select>
  {:else}
    <input id={`${idPrefix}-auth`} value={values.authMode} disabled />
  {/if}
</div>
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
    <option value="operator">Operator-held credentials</option>
    <option
      value="caller"
      disabled={spec.kind === 'plugin' ||
        !['api_key', 'headers', 'static'].includes(values.authMode)}
      >Caller supplies each request</option
    >
  </select>
  <small
    >Caller mode requires X-OLP-Provider-Credential and keeps the endpoint and
    profile fixed. Keep an operator credential for probes and certification.
    Caller secrets are never saved or shared between targets.</small
  >
</div>
<!-- A plugin provider's endpoint is its profile's address, which the server sets. -->
{#if spec.kind !== 'plugin' && (hasCustomEndpoint(spec) || values.profileId)}
  <div class="form-field full">
    <label for={`${idPrefix}-endpoint`}
      >{spec.kind === 'azure_openai'
        ? 'Azure resource endpoint'
        : 'Endpoint'}</label
    >
    <input
      id={`${idPrefix}-endpoint`}
      type="url"
      bind:value={values.endpoint}
      oninput={onChange}
      readonly={endpointReadonly}
      disabled={fieldsDisabled}
      required={hasCustomEndpoint(spec)}
    />
  </div>
{/if}
{#if hasApiVersion(spec)}
  <div class="form-field">
    <label for={`${idPrefix}-version`}>API version</label>
    <input
      id={`${idPrefix}-version`}
      bind:value={values.apiVersion}
      oninput={onChange}
      disabled={fieldsDisabled}
      required={!v1}
      aria-describedby={`${idPrefix}-version-help`}
    />
    {#if v1}<small id={`${idPrefix}-version-help`}
        >Azure v1 does not use a dated API version. Clear this field explicitly
        when migrating.</small
      >{/if}
  </div>
{/if}
{#if hasCloudRegion(spec)}
  <div class="form-field">
    <label for={`${idPrefix}-region`}>Cloud region</label>
    <input
      id={`${idPrefix}-region`}
      bind:value={values.cloudRegion}
      oninput={onChange}
      disabled={fieldsDisabled}
      required
    />
  </div>
{/if}
{#if hasCloudProject(spec)}
  <div class="form-field">
    <label for={`${idPrefix}-project`}>Cloud project</label>
    <input
      id={`${idPrefix}-project`}
      bind:value={values.cloudProject}
      oninput={onChange}
      disabled={fieldsDisabled}
      required
    />
  </div>
{/if}
{#if batchStorageProfile}
  <div class="form-field full">
    <label for={`${idPrefix}-batch-input-uri-prefix`}
      >Batch input URI prefix</label
    >
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
    <label for={`${idPrefix}-batch-output-uri-prefix`}
      >Batch output URI prefix</label
    >
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
    <label for={`${idPrefix}-document-uri-prefixes`}
      >Document URI prefixes</label
    >
    <textarea
      id={`${idPrefix}-document-uri-prefixes`}
      value={documentPrefixes()}
      oninput={(event) => setDocumentPrefixes(event.currentTarget.value)}
      disabled={fieldsDisabled}
      placeholder="gs://bucket/documents/"
      rows="3"></textarea>
    <small
      >One GCS prefix per line. Native document locations outside these prefixes
      are refused, and an empty list grants no document locations. Inline
      documents need no prefix. The cloud region is the Document AI location,
      such as us or eu, and each model names a processor ID.</small
    >
  </div>
{/if}
{#if hasDeployment(spec)}
  <div class="form-field">
    <label for={`${idPrefix}-deployment`}>Cloud deployment</label>
    <input
      id={`${idPrefix}-deployment`}
      bind:value={values.deployment}
      oninput={onChange}
      disabled={fieldsDisabled}
      required={!v1}
    />
  </div>
{/if}
