<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { ServerDraft, ServerField } from './serverDraft';
  let {
    draft = $bindable(),
    errors
  }: {
    draft: ServerDraft;
    errors: FieldErrors<ServerField>;
  } = $props();
</script>

<fieldset>
  <legend>Personal OAuth</legend>
  <label class="check-field">
    <input type="checkbox" bind:checked={draft.oauth} />Require individual
    grants
  </label>
  {#if draft.oauth}
    <FormField label="Client ID" error={errors.clientID}>
      {#snippet control(attributes)}
        <input {...attributes} required bind:value={draft.clientID} />
      {/snippet}
    </FormField>
    <FormField label="Audience" error={errors.audience}>
      {#snippet control(attributes)}
        <input {...attributes} required bind:value={draft.audience} />
      {/snippet}
    </FormField>
    <FormField label="Scopes separated by spaces" error={errors.scopes}>
      {#snippet control(attributes)}
        <input {...attributes} required bind:value={draft.scopes} />
      {/snippet}
    </FormField>
    <FormField
      label="Authorization endpoint"
      error={errors.authorizationEndpoint}
    >
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="url"
          bind:value={draft.authorizationEndpoint}
        />
      {/snippet}
    </FormField>
    <FormField label="Token endpoint" error={errors.tokenEndpoint}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="url"
          required
          bind:value={draft.tokenEndpoint}
        />
      {/snippet}
    </FormField>
    <FormField
      label="Device authorization endpoint"
      error={errors.deviceEndpoint}
    >
      {#snippet control(attributes)}
        <input {...attributes} type="url" bind:value={draft.deviceEndpoint} />
      {/snippet}
    </FormField>
    <FormField label="Entra delegated issuer" error={errors.entraIssuer}>
      {#snippet control(attributes)}
        <input {...attributes} bind:value={draft.entraIssuer} />
      {/snippet}
    </FormField>
    <FormField label="Entra client secret" error={errors.entraSecret}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="password"
          autocomplete="new-password"
          bind:value={draft.entraSecret}
        />
      {/snippet}
    </FormField>
  {/if}
</fieldset>
<fieldset>
  <legend>Delegation</legend>
  <label class="check-field">
    <input type="checkbox" bind:checked={draft.delegation} />Enable delegation
  </label>
  {#if draft.delegation}
    <FormField label="Issuer" error={errors.delegationIssuer}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="url"
          required
          bind:value={draft.delegationIssuer}
        />
      {/snippet}
    </FormField>
    <FormField label="Audience" error={errors.delegationAudience}>
      {#snippet control(attributes)}
        <input {...attributes} required bind:value={draft.delegationAudience} />
      {/snippet}
    </FormField>
    <FormField label="PKCS8 Ed25519 private key" error={errors.privateKey} full>
      {#snippet control(attributes)}
        <textarea
          {...attributes}
          autocomplete="off"
          bind:value={draft.privateKey}></textarea>
      {/snippet}
    </FormField>
  {/if}
</fieldset>
