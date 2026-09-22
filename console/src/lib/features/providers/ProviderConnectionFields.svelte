<script lang="ts" generics="Values extends ProviderEditValues">
  import { createQuery } from '@tanstack/svelte-query';
  import { listProviderProfiles } from './profiles';
  import type { ProviderKindCapability } from './models';
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
    queryKey: ['provider-profiles'],
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
{#if hasCustomEndpoint(spec) || values.profileId}
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
