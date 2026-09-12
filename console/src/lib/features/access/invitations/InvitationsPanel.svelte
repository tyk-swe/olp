<script lang="ts">
  import { invitationKeys } from '$lib/features/access/invitations/invitationKeys';

  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import {
    createInvitation,
    listInvitationPage,
    revokeInvitation,
    type Invitation,
    type InvitationSecret
  } from '$lib/features/access/api';
  import { copyText } from '$lib/clipboard';
  import { errorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { FIXED_ROLES } from '$lib/features/access/session/authorization';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import { formatDate } from '$lib/format';

  // The API defaults to seven days and rejects anything over thirty.
  const DEFAULT_EXPIRY_HOURS = 7 * 24;
  const expiryChoices = [
    { hours: 24, label: '24 hours' },
    { hours: 3 * 24, label: '3 days' },
    { hours: DEFAULT_EXPIRY_HOURS, label: '7 days' },
    { hours: 14 * 24, label: '14 days' },
    { hours: 30 * 24, label: '30 days' }
  ];

  /** The id is the fallback for an operator whose account no longer exists. */
  function invitedBy(invitation: Invitation): string {
    return invitation.invited_by_email ?? invitation.invited_by;
  }

  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let email = $state('');
  let role = $state('developer');
  let expiresInHours = $state(DEFAULT_EXPIRY_HOURS);
  let invitationSecret = $state<InvitationSecret | null>(null);
  let copied = $state(false);
  let copyError = $state('');

  const invitations = createQuery(() => ({
    queryKey: invitationKeys.page(pagination.cursor),
    queryFn: () => listInvitationPage(pagination.cursor)
  }));

  onDestroy(() => {
    invitationSecret = null;
  });

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    error = notice = '';
    try {
      await action();
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function invite(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage) return;
    if (!email.trim() || !email.includes('@')) {
      error = 'Enter a valid email address.';
      return;
    }
    await run('invite', async () => {
      invitationSecret = await createInvitation(
        email.trim(),
        role,
        expiresInHours
      );
      email = '';
      await invitations.refetch();
    });
  }

  async function removeInvitation(id: string, invitationEmail: string) {
    if (!confirm(`Revoke the invitation for ${invitationEmail}?`)) return;
    await run(`invitation-${id}`, async () => {
      await revokeInvitation(id);
      await invitations.refetch();
      notice = 'Invitation revoked.';
    });
  }

  function invitationLink() {
    if (!invitationSecret) return '';
    // Keep the one-time token in the URL fragment so it is never sent in an
    // HTTP request, Referer header, or static-console access log.
    return `${window.location.origin}/invitations/accept#token=${encodeURIComponent(invitationSecret.token)}`;
  }

  async function copyInvitation() {
    if (!invitationSecret) return;
    if (!(await copyText(invitationLink()))) {
      copied = false;
      copyError =
        'Clipboard access is unavailable. Copy this invitation link manually.';
      return;
    }
    copyError = '';
    copied = true;
  }
</script>

{#if invitationSecret}
  <SecretDialog
    eyebrow="Invitation created"
    title="Copy the invitation link now."
    description={`The token is displayed once and expires at ${formatDate(invitationSecret.invitation.expires_at)}.`}
    onClose={() => {
      invitationSecret = null;
      copied = false;
      copyError = '';
    }}
  >
    {#snippet children(close)}
      <code class="invitation-token">{invitationLink()}</code>
      {#if copyError}<div class="inline-problem" role="alert">
          {copyError}
        </div>{/if}
      <div class="dialog-actions">
        <button
          class="button button-secondary"
          type="button"
          onclick={copyInvitation}
          >{copied ? 'Link copied' : 'Copy invitation link'}</button
        ><button
          class="button button-primary"
          type="button"
          data-autofocus
          onclick={close}>I have shared it</button
        >
        <span class="sr-only" aria-live="polite">
          {copied ? 'Invitation link copied to clipboard.' : ''}
        </span>
      </div>
    {/snippet}
  </SecretDialog>
{/if}

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if !canManage}
  <ReadOnlyNote>
    Your role can view invitations but not create or revoke them.
  </ReadOnlyNote>
{/if}
<section class="card invite-panel" aria-labelledby="invite-heading">
  <div>
    <p class="eyebrow">New invitation</p>
    <h2 id="invite-heading">Invite by email</h2>
    <p>The acceptance token is shown once. No email service is required.</p>
  </div>
  <form onsubmit={invite} novalidate>
    <label
      ><span>Email address</span><input
        type="email"
        autocomplete="email"
        bind:value={email}
        placeholder="person@example.com"
        disabled={!canManage}
      /></label
    >
    <label
      ><span>Role</span><select bind:value={role} disabled={!canManage}
        >{#each FIXED_ROLES as fixedRole (fixedRole)}<option value={fixedRole}
            >{fixedRole}</option
          >{/each}</select
      ></label
    >
    <label
      ><span>Expires in</span><select
        bind:value={expiresInHours}
        disabled={!canManage}
        >{#each expiryChoices as choice (choice.hours)}<option
            value={choice.hours}>{choice.label}</option
          >{/each}</select
      ></label
    >
    <button
      class="button button-primary"
      type="submit"
      disabled={!canManage || busy === 'invite'}
      >{busy === 'invite' ? 'Creating…' : 'Create invitation'}</button
    >
  </form>
</section>

{#if invitations.isPending}
  <div class="loading-state" role="status">Loading invitation history…</div>
{:else if invitations.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(invitations.error)}
  </div>
{:else if !invitations.data?.items.length && pagination.history.length === 0}
  <section class="card empty-state">
    <p>No invitations have been created.</p>
  </section>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          ><th>Email / invited by</th><th>Role</th><th>Status</th><th
            >Expires / created</th
          ><th><span class="sr-only">Actions</span></th></tr
        ></thead
      >
      <tbody>
        {#each invitations.data?.items ?? [] as invitation (invitation.id)}
          <tr>
            <td
              ><strong>{invitation.email}</strong><br /><small
                >Invited by {invitedBy(invitation)}</small
              ></td
            ><td><span class="badge">{invitation.role}</span></td>
            <td
              ><span
                class:success={invitation.status === 'accepted'}
                class:warning={invitation.status === 'pending'}
                class:danger={invitation.status === 'revoked'}
                class="badge">{invitation.status}</span
              >{#if invitation.accepted_at}<br /><small
                  >Accepted {formatDate(
                    invitation.accepted_at
                  )}{#if invitation.accepted_by_email}
                    by {invitation.accepted_by_email}{/if}</small
                >{/if}{#if invitation.revoked_at}<br /><small
                  >Revoked {formatDate(
                    invitation.revoked_at
                  )}{#if invitation.revoked_by_email}
                    by {invitation.revoked_by_email}{/if}</small
                >{/if}</td
            >
            <td
              >{formatDate(invitation.expires_at)}<br /><small
                >Created {formatDate(invitation.created_at)}</small
              ></td
            >
            <td
              >{#if canManage && invitation.status === 'pending'}<button
                  class="button button-secondary danger-button"
                  type="button"
                  onclick={() =>
                    removeInvitation(invitation.id, invitation.email)}
                  disabled={Boolean(busy)}>Revoke</button
                >{/if}</td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(pagination, invitations.data?.nextCursor)}
    label="Invitation pages"
  />
{/if}

<style>
  h2 {
    margin: 0;
    font-size: 1rem;
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
  .danger-button {
    color: var(--danger);
  }
  td small {
    color: var(--foreground-muted);
  }
  .invitation-token {
    display: block;
    overflow-x: auto;
    padding: 0.85rem;
    border-radius: var(--radius-control);
    background: var(--code-bg);
    color: var(--code-foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  .dialog-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.65rem;
  }
  @media (max-width: 64rem) {
    .invite-panel {
      display: grid;
    }
  }
  @media (max-width: 42rem) {
    .invite-panel form,
    .dialog-actions {
      display: grid;
    }
  }
</style>
