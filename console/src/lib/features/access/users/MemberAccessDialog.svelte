<script lang="ts">
  import { untrack } from 'svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import { FIXED_ROLES } from '#lib/features/access/session/authorization.js';
  import type { User } from './api';
  import { ROLE_DETAILS, SCOPE_DETAILS } from './roles';

  let {
    user,
    onSave,
    onClose
  }: {
    user: User;
    onSave: (patch: Pick<User, 'role' | 'access_scope'>) => Promise<boolean>;
    onClose: () => void;
  } = $props();

  let role = $state(untrack(() => user.role));
  let scope = $state(untrack(() => user.access_scope));
  let busy = $state(false);
  const dirty = $derived(role !== user.role || scope !== user.access_scope);
</script>

<Dialog
  title="Change access for {user.display_name}"
  description="They’ll be signed out everywhere and pick up the new access when they sign in again. Their API keys keep working."
  {onClose}
>
  {#snippet children(close)}
    <form
      class="access-form"
      onsubmit={async (event) => {
        event.preventDefault();
        if (!dirty || busy) return;
        busy = true;
        const saved = await onSave({ role, access_scope: scope });
        busy = false;
        if (saved) close();
      }}
    >
      <fieldset>
        <legend>Role</legend>
        {#each FIXED_ROLES as option (option)}
          <label class="option">
            <input type="radio" name="role" value={option} bind:group={role} />
            <span
              ><strong>{ROLE_DETAILS[option].label}</strong>
              <small>{ROLE_DETAILS[option].summary}</small></span
            >
          </label>
        {/each}
      </fieldset>
      <fieldset>
        <legend>Where it applies</legend>
        {#each ['global', 'assigned'] as const as option (option)}
          <label class="option">
            <input
              type="radio"
              name="scope"
              value={option}
              bind:group={scope}
            />
            <span
              ><strong>{SCOPE_DETAILS[option].label}</strong>
              <small>{SCOPE_DETAILS[option].summary}</small></span
            >
          </label>
        {/each}
      </fieldset>
      <div class="page-actions">
        <button class="button button-secondary" type="button" onclick={close}
          >Cancel</button
        >
        <button
          class="button button-primary"
          type="submit"
          disabled={!dirty || busy}
          >{busy ? 'Saving…' : 'Save and sign them out'}</button
        >
      </div>
    </form>
  {/snippet}
</Dialog>

<style>
  .access-form {
    display: grid;
    gap: 1.25rem;
  }
  fieldset {
    display: grid;
    gap: 0.4rem;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    margin-bottom: 0.4rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .option {
    display: flex;
    align-items: start;
    gap: 0.75rem;
    padding: 0.65rem 0.8rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-control);
    cursor: pointer;
  }
  .option:has(input:checked) {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .option input {
    margin-top: 0.2rem;
  }
  .option span {
    display: grid;
    gap: 0.1rem;
  }
  .option small {
    color: var(--foreground-muted);
  }
  .page-actions {
    justify-content: flex-end;
  }
</style>
