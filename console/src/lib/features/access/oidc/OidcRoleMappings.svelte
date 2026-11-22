<script lang="ts">
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';

  let {
    emailMappings = $bindable(''),
    groupMappings = $bindable(''),
    canManage,
    oidcEnabled,
    busy,
    busyKey,
    onLinkIdentity
  }: {
    emailMappings: string;
    groupMappings: string;
    canManage: boolean;
    oidcEnabled: boolean;
    busy: boolean;
    busyKey: string;
    onLinkIdentity: () => void;
  } = $props();
</script>

<section class="card mapping-form" aria-labelledby="mapping-heading">
  <h2 id="mapping-heading">Roles from your identity provider</h2>
  <p class="muted">
    One mapping per line in <code>claim-value=role</code> form. Email mappings take
    precedence over group mappings and the default.
  </p>
  <div class="form-field">
    <label for="email-mappings">Email mappings</label><textarea
      id="email-mappings"
      bind:value={emailMappings}
      disabled={!canManage}
      placeholder="owner@example.com=owner"></textarea>
  </div>
  <div class="form-field">
    <label for="group-mappings">Group mappings</label><textarea
      id="group-mappings"
      bind:value={groupMappings}
      disabled={!canManage}
      placeholder="platform-team=operator"></textarea>
  </div>
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
      >{busyKey === 'oidc-link' ? 'Redirecting…' : 'Link my identity'}</button
    ><button
      class="button button-primary"
      type="submit"
      disabled={!canManage || busy}
      >{busyKey === 'oidc-save' ? 'Validating…' : 'Save and validate'}</button
    >
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
