<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { OidcField } from './oidcDraft';

  let {
    emailMappings = $bindable(''),
    groupMappings = $bindable(''),
    canManage,
    oidcEnabled,
    busy,
    busyKey,
    errors,
    onLinkIdentity
  }: {
    emailMappings: string;
    groupMappings: string;
    canManage: boolean;
    oidcEnabled: boolean;
    busy: boolean;
    busyKey: string;
    errors: FieldErrors<OidcField>;
    onLinkIdentity: () => void;
  } = $props();
</script>

<section class="card mapping-form" aria-labelledby="mapping-heading">
  <h2 id="mapping-heading">Roles from your identity provider</h2>
  <p class="muted">
    One mapping per line in <code>claim-value=role</code> form. Email mappings take
    precedence over group mappings and the default.
  </p>
  <FormField label="Email mappings" error={errors.emailMappings}>
    {#snippet control(attributes)}
      <textarea
        {...attributes}
        bind:value={emailMappings}
        disabled={!canManage}
        placeholder="owner@example.com=owner"></textarea>
    {/snippet}
  </FormField>
  <FormField label="Group mappings" error={errors.groupMappings}>
    {#snippet control(attributes)}
      <textarea
        {...attributes}
        bind:value={groupMappings}
        disabled={!canManage}
        placeholder="platform-team=operator"></textarea>
    {/snippet}
  </FormField>
  {#if !oidcEnabled}
    <ReadOnlyNote role={undefined}>
      Save with OIDC enabled before linking your identity.
    </ReadOnlyNote>
  {/if}
  <div class="oidc-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={onLinkIdentity}
      disabled={!oidcEnabled || busy}
    >
      {busyKey === 'oidc-link' ? 'Redirecting…' : 'Link my identity'}
    </button><button
      class="button button-primary"
      type="submit"
      disabled={!canManage || busy}
    >
      {busyKey === 'oidc-save' ? 'Validating…' : 'Save and validate'}
    </button>
  </div>
</section>

<style>
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .mapping-form {
    display: grid;
    align-content: start;
    gap: 1rem;
    padding: 1.5rem;
  }
  .mapping-form h2,
  .mapping-form p {
    margin: 0;
  }
  .oidc-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.65rem;
  }
  code {
    font-size: var(--text-caption);
  }
  @media (max-width: 42rem) {
    .oidc-actions {
      display: grid;
    }
  }
</style>
