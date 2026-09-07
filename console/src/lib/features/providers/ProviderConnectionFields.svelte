<script lang="ts" generics="Values extends ProviderEditValues">
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
</script>

<div class="form-field">
  <label for={`${idPrefix}-name`}>Provider name</label>
  <input
    id={`${idPrefix}-name`}
    bind:value={values.name}
    oninput={onChange}
    {disabled}
    required
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-auth`}>Authentication</label>
  {#if authEditable}
    <select
      id={`${idPrefix}-auth`}
      bind:value={values.authMode}
      onchange={onChange}
      {disabled}
    >
      {#each authOptionsFor(spec) as option (option[0])}
        <option value={option[0]}>{option[1]}</option>
      {/each}
    </select>
  {:else}
    <input id={`${idPrefix}-auth`} value={values.authMode} disabled />
  {/if}
</div>
{#if hasCustomEndpoint(spec)}
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
      {disabled}
      required
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
      {disabled}
      required
    />
  </div>
{/if}
{#if hasCloudRegion(spec)}
  <div class="form-field">
    <label for={`${idPrefix}-region`}>Cloud region</label>
    <input
      id={`${idPrefix}-region`}
      bind:value={values.cloudRegion}
      oninput={onChange}
      {disabled}
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
      {disabled}
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
      {disabled}
      required
    />
  </div>
{/if}
