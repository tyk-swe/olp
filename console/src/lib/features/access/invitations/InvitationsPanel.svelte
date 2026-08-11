<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import {
    createInvitation,
    listInvitationPage,
    revokeInvitation,
    type InvitationSecret
  } from '$lib/api/management/access';
  import { copyText } from '$lib/clipboard';
  import { errorMessage as accessErrorMessage } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import { FIXED_ROLES } from '../shared';

  let cursor = $state<string | undefined>();
  let history = $state<Array<string | undefined>>([]);
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let email = $state('');
  let role = $state('developer');
  let invitationSecret = $state<InvitationSecret | null>(null);
  let copied = $state(false);
  let copyError = $state('');

  const invitations = createQuery(() => ({
    queryKey: ['invitation-page', cursor ?? 'first'],
    queryFn: () => listInvitationPage(cursor)
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
      error = accessErrorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function invite(event: SubmitEvent) {
    event.preventDefault();
    if (!email.trim() || !email.includes('@')) {
      error = 'Enter a valid email address.';
      return;
    }
    await run('invite', async () => {
      invitationSecret = await createInvitation(email.trim(), role);
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

  function nextPage() {
    const next = invitations.data?.nextCursor;
    if (!next) return;
    history = [...history, cursor];
    cursor = next;
  }

  function previousPage() {
    cursor = history.at(-1);
    history = history.slice(0, -1);
  }
</script>

{#if invitationSecret}
  <SecretDialog
    eyebrow="Invitation created"
    title="Copy the invitation link now."
    description={`The token is displayed once and expires at ${new Date(invitationSecret.invitation.expires_at).toLocaleString()}.`}
    onClose={() => {
      invitationSecret = null;
      copied = false;
      copyError = '';
    }}
  >
    {#snippet children(close)}
      <code class="invitation-token">{invitationSecret!.token}</code>
      {#if copyError}<div class="inline-problem" role="alert">{copyError}</div>
        <code class="invitation-token">{invitationLink()}</code>{/if}
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
      </div>
    {/snippet}
  </SecretDialog>
{/if}

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

<section class="card invite-panel" aria-labelledby="invite-heading">
  <div>
    <p class="eyebrow">New invitation</p>
    <h2 id="invite-heading">Invite by email</h2>
    <p>The acceptance token is shown once. No email service is required.</p>
  </div>
  <form onsubmit={invite}>
    <label
      ><span>Email address</span><input
        type="email"
        autocomplete="email"
        bind:value={email}
        placeholder="person@example.com"
      /></label
    >
    <label
      ><span>Role</span><select bind:value={role}
        >{#each FIXED_ROLES as fixedRole (fixedRole)}<option value={fixedRole}
            >{fixedRole}</option
          >{/each}</select
      ></label
    >
    <button
      class="button button-primary"
      type="submit"
      disabled={busy === 'invite'}
      >{busy === 'invite' ? 'Creating…' : 'Create invitation'}</button
    >
  </form>
</section>

{#if invitations.isPending}
  <div class="loading-state" role="status">Loading invitation history…</div>
{:else if invitations.isError}
  <div class="inline-problem" role="alert">
    {accessErrorMessage(invitations.error)}
  </div>
{:else if !invitations.data?.items.length && history.length === 0}
  <section class="card empty-state">
    <p>No invitations have been created.</p>
  </section>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead
        ><tr
          ><th>Email</th><th>Role</th><th>Status</th><th>Expires</th><th
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each invitations.data?.items ?? [] as invitation (invitation.id)}
          <tr>
            <td>{invitation.email}</td><td
              ><span class="badge">{invitation.role}</span></td
            >
            <td
              ><span
                class:success={invitation.status === 'accepted'}
                class:warning={invitation.status === 'pending'}
                class:danger={invitation.status === 'revoked'}
                class="badge">{invitation.status}</span
              ></td
            >
            <td>{new Date(invitation.expires_at).toLocaleString()}</td>
            <td
              >{#if invitation.status === 'pending'}<button
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
    page={history.length + 1}
    hasPrevious={history.length > 0}
    hasNext={Boolean(invitations.data?.nextCursor)}
    onPrevious={previousPage}
    onNext={nextPage}
    label="Invitation pages"
  />
{/if}

<style>
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  h2 {
    margin: 0;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .invite-panel {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: 2rem;
    margin-bottom: 1rem;
    padding: 1.25rem;
  }
  .invite-panel p {
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
    gap: 0.3rem;
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  .invite-panel input,
  .invite-panel select {
    min-height: 2.5rem;
    padding: 0.5rem 0.65rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .danger-button {
    color: var(--danger);
  }
  .invitation-token {
    display: block;
    overflow-x: auto;
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
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
