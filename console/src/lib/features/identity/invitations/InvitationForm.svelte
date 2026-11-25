<script lang="ts">
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';

  type InviteInput = {
    email: string;
    role: string;
    expiresInHours: number;
  };

  let {
    canManage,
    pending,
    failed,
    loginEnabled,
    onRetry,
    creating,
    onSubmit
  }: {
    canManage: boolean;
    pending: boolean;
    failed: boolean;
    loginEnabled: boolean;
    onRetry: () => void;
    creating: boolean;
    onSubmit: (input: InviteInput) => Promise<boolean>;
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

  let email = $state('');
  let role = $state('developer');
  let expiresInHours = $state(DEFAULT_EXPIRY_HOURS);

  usePrincipalLifetime(() => {
    email = '';
    role = 'developer';
    expiresInHours = DEFAULT_EXPIRY_HOURS;
  });

  async function invite(event: SubmitEvent) {
    event.preventDefault();
    if (await onSubmit({ email, role, expiresInHours })) email = '';
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
    <label>
      <span>Email address</span><input
        type="email"
        autocomplete="email"
        bind:value={email}
        placeholder="person@example.com"
        disabled={!canManage || !loginEnabled}
      />
    </label>
    <label>
      <span>Role</span><select
        bind:value={role}
        disabled={!canManage || !loginEnabled}
      >
        {#each FIXED_ROLES as fixedRole (fixedRole)}<option value={fixedRole}>
            {fixedRole}
          </option>{/each}
      </select>
    </label>
    <label>
      <span>Expires in</span><select
        bind:value={expiresInHours}
        disabled={!canManage || !loginEnabled}
      >
        {#each expiryChoices as choice (choice.hours)}<option
            value={choice.hours}
          >
            {choice.label}
          </option>{/each}
      </select>
    </label>
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
    align-items: end;
    gap: 0.65rem;
  }
  .invite-panel label {
    display: grid;
    gap: 0.4rem;
    font-weight: 500;
  }
  .invite-panel input,
  .invite-panel select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .invite-panel input:hover,
  .invite-panel select:hover {
    border-color: var(--border-strong);
  }
  .invite-panel input::placeholder {
    color: var(--foreground-muted);
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
  }
</style>
