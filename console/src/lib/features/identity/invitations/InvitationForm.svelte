<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import { focusFormError } from '#lib/editing/focusError.js';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';
  import type {
    InvitationDraft,
    InvitationField
  } from './invitationValidation';

  let {
    canManage,
    pending,
    failed,
    loginEnabled,
    onRetry,
    creating,
    errors,
    onSubmit
  }: {
    canManage: boolean;
    pending: boolean;
    failed: boolean;
    loginEnabled: boolean;
    onRetry: () => void;
    creating: boolean;
    errors: FieldErrors<InvitationField>;
    onSubmit: (draft: InvitationDraft) => Promise<boolean>;
  } = $props();

  // The API defaults to seven days and rejects anything over thirty.
  const DEFAULT_EXPIRY_HOURS = 7 * 24;
  const expiryChoices = [
    { hours: 24, label: '24 hours' },
    { hours: 3 * 24, label: '3 days' },
    { hours: DEFAULT_EXPIRY_HOURS, label: '7 days' },
    { hours: 14 * 24, label: '14 days' },
    { hours: 30 * 24, label: '30 days' }
  ];

  function newDraft(): InvitationDraft {
    return {
      email: '',
      role: 'developer',
      expiresInHours: DEFAULT_EXPIRY_HOURS
    };
  }

  let draft = $state(newDraft());

  usePrincipalLifetime(() => {
    draft = newDraft();
  });

  async function invite(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    if (await onSubmit(draft)) draft.email = '';
    else await focusFormError(form);
  }
</script>

<section class="card invite-panel" aria-labelledby="invite-heading">
  <div>
    <p class="eyebrow">New invitation</p>
    <h2 id="invite-heading">Invite by email</h2>
    <p>The acceptance token is shown once. No email service is required.</p>
  </div>
  {#if pending}
    <p class="inline-status" role="status">Checking invitation availability…</p>
  {:else if failed}
    <div role="alert">
      Invitation availability could not be checked.
      <button class="text-button" type="button" onclick={onRetry}>Retry</button>
    </div>
  {:else if !loginEnabled}
    <p class="inline-status" role="status">
      Password invitations require local sign-in. Configure an OIDC role mapping
      and ask the member to sign in with single sign-on.
    </p>
  {/if}
  <form onsubmit={invite} novalidate>
    <FormField label="Email address" error={errors.email}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="email"
          autocomplete="email"
          bind:value={draft.email}
          placeholder="person@example.com"
          disabled={!canManage || !loginEnabled}
        />
      {/snippet}
    </FormField>
    <FormField label="Role" error={errors.role}>
      {#snippet control(attributes)}
        <select
          {...attributes}
          bind:value={draft.role}
          disabled={!canManage || !loginEnabled}
        >
          {#each FIXED_ROLES as fixedRole (fixedRole)}<option value={fixedRole}>
              {fixedRole}
            </option>{/each}
        </select>
      {/snippet}
    </FormField>
    <FormField label="Expires in" error={errors.expiresInHours}>
      {#snippet control(attributes)}
        <select
          {...attributes}
          bind:value={draft.expiresInHours}
          disabled={!canManage || !loginEnabled}
        >
          {#each expiryChoices as choice (choice.hours)}<option
              value={choice.hours}
            >
              {choice.label}
            </option>{/each}
        </select>
      {/snippet}
    </FormField>
    <button
      class="button button-primary"
      type="submit"
      disabled={!canManage || !loginEnabled || creating}
    >
      {creating ? 'Creating…' : 'Create invitation'}
    </button>
  </form>
</section>

<style>
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .invite-panel {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: 2rem;
    margin-bottom: 1rem;
    padding: 1.5rem;
  }
  .invite-panel h2 + p {
    margin: 0.4rem 0 0;
    color: var(--foreground-muted);
  }
  .invite-panel form {
    display: flex;
    align-items: start;
    gap: 0.65rem;
  }
  .invite-panel form > :global(.form-field) {
    min-width: 7.75rem;
  }
  .invite-panel form button {
    margin-top: calc(1lh + 0.4rem);
    padding-block: 0;
    line-height: inherit;
  }
  @media (max-width: 64rem) {
    .invite-panel {
      display: grid;
    }
  }
  @media (max-width: 42rem) {
    .invite-panel form {
      display: grid;
    }
    .invite-panel form button {
      margin-top: 0;
    }
  }
</style>
