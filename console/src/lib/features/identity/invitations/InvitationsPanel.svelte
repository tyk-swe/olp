<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import {
    useMutationIntents,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import InvitationForm from './InvitationForm.svelte';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { invitationKeys } from '#lib/features/identity/invitations/invitationKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import {
    createInvitation,
    listInvitationPage,
    revokeInvitation,
    type Invitation,
    type InvitationSecret
  } from '#lib/features/identity/invitations/api.js';
  import { copyText } from '#lib/clipboard.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import SecretDialog from '#lib/components/SecretDialog.svelte';
  import { formatDate } from '#lib/format.js';

  const write = useAction();
  const intents = useMutationIntents();

  /** The id is the fallback for an operator whose account no longer exists. */
  function invitedBy(invitation: Invitation): string {
    return invitation.invited_by_email ?? invitation.invited_by;
  }

  const capabilities = useServiceCapabilities();
  const access = useRole();
  const canManage = $derived(access.can('users.manage'));
  const pagination = $state(emptyCursorHistory());
  let invitationSecret = $state<InvitationSecret | null>(null);
  let copied = $state(false);
  let copyError = $state('');

  const invitations = createQuery(() => ({
    queryKey: invitationKeys.page(pagination.cursor),
    queryFn: () => listInvitationPage(pagination.cursor)
  }));

  const captureOwner = usePrincipalLifetime(() => {
    invitationSecret = null;
    copied = false;
    copyError = '';
  });

  function invite(input: {
    email: string;
    role: string;
    expiresInHours: number;
  }) {
    if (!canManage || !capabilities.localLoginEnabled)
      return Promise.resolve(false);
    if (!input.email.trim() || !input.email.includes('@')) {
      write.error = 'Enter a valid email address.';
      return Promise.resolve(false);
    }
    return write.run(
      () =>
        intents.submit(
          'create',
          {
            email: input.email.trim(),
            role: input.role,
            expiresInHours: input.expiresInHours
          },
          (submission, requestId) =>
            createInvitation(
              submission.email,
              submission.role,
              submission.expiresInHours,
              requestId
            )
        ),
      async (result, current) => {
        invitationSecret = result;
        notify('Invitation created.');
        const observation = await readResult(() => invitations.refetch());
        const failed = observation.ok
          ? observation.value?.isError
            ? observation.value.error
            : null
          : observation.error;
        if (failed !== null && current())
          write.error = `Invitation created, but refreshing failed. ${errorMessage(failed)}`;
      },
      { key: 'invite' }
    );
  }

  async function removeInvitation(id: string, invitationEmail: string) {
    if (
      !(await confirmAction({
        title: `Revoke the invitation for ${invitationEmail}?`,
        body: 'The link in their email stops working.',
        confirmLabel: 'Revoke invitation',
        danger: true
      }))
    )
      return;
    await write.run(
      () =>
        intents.submit(`invitation-${id}`, id, (input, requestId) =>
          revokeInvitation(input, requestId)
        ),
      async () => {
        await invitations.refetch();
        notify('Invitation revoked.');
      },
      { key: `invitation-${id}` }
    );
  }

  function invitationLink() {
    if (!invitationSecret) return '';
    // Keep the one-time token in the URL fragment so it is never sent in an
    // HTTP request, Referer header, or static-console access log.
    return `${window.location.origin}/invitations/accept#token=${encodeURIComponent(invitationSecret.token)}`;
  }

  async function copyInvitation() {
    if (!invitationSecret) return;
    const isCurrent = captureOwner();
    const didCopy = await copyText(invitationLink());
    if (!isCurrent()) return;
    if (!didCopy) {
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

{#if write.error}<div class="inline-problem" role="alert">
    {write.error}
  </div>{/if}

{#if !canManage}
  <ReadOnlyNote>
    Your role can view invitations but not create or revoke them.
  </ReadOnlyNote>
{/if}
<InvitationForm
  {canManage}
  pending={capabilities.pending}
  failed={Boolean(capabilities.error)}
  loginEnabled={capabilities.localLoginEnabled}
  onRetry={() => capabilities.retry()}
  creating={write.key === 'invite'}
  onSubmit={invite}
/>

<QueryView
  query={invitations}
  loading="Loading invitation history…"
  failure={errorMessage(invitations.error)}
  emptyWhen={!invitations.data?.items.length && pagination.history.length === 0}
>
  {#snippet empty()}
    <EmptyState title="No invitations yet"
      >Invite someone by email and choose their role.</EmptyState
    >
  {/snippet}
  <ScrollTable label="Invitations">
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
                  class="button button-secondary button-danger"
                  type="button"
                  onclick={() =>
                    removeInvitation(invitation.id, invitation.email)}
                  disabled={write.busy}>Revoke</button
                >{/if}</td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
  <CursorPagination
    {...cursorPaginationProps(pagination, invitations.data?.nextCursor)}
    label="Invitation pages"
  />
</QueryView>

<style>
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
  @media (max-width: 42rem) {
    .dialog-actions {
      display: grid;
    }
  }
</style>
