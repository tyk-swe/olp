<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { MCPServer } from './api';
  import type { ServerPricingField, ServerPricingForm } from './form';
  let {
    pricing = $bindable(),
    tools,
    errors
  }: {
    pricing: ServerPricingForm;
    tools: MCPServer['catalog']['tools'];
    errors: FieldErrors<ServerPricingField>;
  } = $props();
</script>

<fieldset>
  <legend>Pricing and limits</legend>
  <FormField label="Price per call" error={errors.unitPrice}>
    {#snippet control(attributes)}
      <input {...attributes} bind:value={pricing.unitPrice} />
    {/snippet}
  </FormField>
  <p class="inline-status">Blank means unpriced. Use 0 for free calls</p>
  {#each tools as tool (tool.name)}
    <FormField
      label={`${tool.name} price`}
      error={errors[`toolPrice:${tool.name}`]}
    >
      {#snippet control(attributes)}
        <input {...attributes} bind:value={pricing.toolPrices[tool.name]} />
      {/snippet}
    </FormField>
  {/each}
  <FormField label="Calls per minute" error={errors.callsPerMinute}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        inputmode="numeric"
        bind:value={pricing.callsPerMinute}
      />
    {/snippet}
  </FormField>
  <FormField label="Concurrent calls" error={errors.maxConcurrency}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        inputmode="numeric"
        bind:value={pricing.maxConcurrency}
      />
    {/snippet}
  </FormField>
  <FormField
    label="Calls per minute per key"
    error={errors.perKeyCallsPerMinute}
  >
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        inputmode="numeric"
        bind:value={pricing.perKeyCallsPerMinute}
      />
    {/snippet}
  </FormField>
  <FormField
    label="Concurrent calls per key"
    error={errors.perKeyMaxConcurrency}
  >
    {#snippet control(attributes)}
      <input
        {...attributes}
        required
        inputmode="numeric"
        bind:value={pricing.perKeyMaxConcurrency}
      />
    {/snippet}
  </FormField>
</fieldset>
