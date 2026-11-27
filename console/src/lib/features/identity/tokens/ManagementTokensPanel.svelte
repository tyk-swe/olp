<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import ManagementTokenForm, {
    type TokenFormInput
  } from './ManagementTokenForm.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { managementTokenKeys } from '#lib/features/identity/tokens/tokenKeys.js';
  import {
    createManagementToken,
    listManagementTokenPage,
    revokeManagementToken,
    type ManagementToken,
    type ManagementTokenSecret
  } from '#lib/features/identity/tokens/api.js';
  import { listProjects, projectKeys } from '#lib/features/access/index.js';
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
  const createIntent = useMutationIntent();
  const revokeIntent = useMutationIntent();

  function status(token: ManagementToken): string {
    if (token.revoked_at) return 'revoked';
    if (new Date(token.expires_at).getTime() <= Date.now()) return 'expired';
    return 'active';
  }

  const access = useRole();
  const isOwner = $derived(access.allows('POST /api/v1/management-tokens'));
  const pagination = $state(emptyCursorHistory());
  let creating = $state(false);
  let tokenSecret = $state<ManagementTokenSecret | null>(null);
  let copied = $state(false);
  let copyError = $state('');

  const tokens = createQuery(() => ({
    queryKey: managementTokenKeys.page(pagination.cursor),
    queryFn: () => listManagementTokenPage(pagination.cursor),
    enabled: isOwner
  }));
  const projects = createQuery(() => ({
    queryKey: projectKeys.all,
    queryFn: ({ signal }) => listProjects(signal),
    enabled: isOwner
  }));
  const projectNames = $derived(
    new Map((projects.data ?? []).map((item) => [item.id, item.name]))
  );

  const captureOwner = usePrincipalLifetime(() => {
    tokenSecret = null;
    copied = false;
    copyError = '';
    creating = false;
  });

  function projectScopeLabel(token: ManagementToken): string {
    if (token.all_projects) return 'All projects';
    const names = (token.project_ids ?? [])
      .map((id) => projectNames.get(id) ?? id)
      .join(', ');
    return names || 'No projects';
  }

  function create(input: TokenFormInput) {
    if (!isOwner) return Promise.resolve(false);
    if (!input.name.trim()) {
      write.error = 'Enter a token name.';
      return Promise.resolve(false);
    }
    if (!input.scopes.length) {
      write.error = 'Select at least one scope.';
      return Promise.resolve(false);
    }
    if (!input.allProjects && !input.projectIds.length) {
      write.error = 'Select at least one project, or choose all projects.';
      return Promise.resolve(false);
    }
    return write.run(
      () =>
        createIntent.submitBuilt(
          {
            name: input.name.trim(),
            scopes: input.scopes,
            expiresInDays: input.expiresInDays,
            projectIds: input.allProjects ? undefined : input.projectIds
          },
          () => ({
            name: input.name.trim(),
            scopes: input.scopes,
            expiresAt: new Date(
              Date.now() + input.expiresInDays * 24 * 60 * 60 * 1000
            ).toISOString(),
            projectIds: input.allProjects ? undefined : input.projectIds
          }),
          (submission, requestId) =>
            createManagementToken(
              submission.name,
              submission.scopes,
              submission.expiresAt,
              submission.projectIds,
              requestId
            )
        ),
      async (result, current) => {
        tokenSecret = result;
        creating = false;
        const observation = await readResult(() => tokens.refetch());
        const failed = observation.ok
          ? observation.value?.isError
            ? observation.value.error
            : null
          : observation.error;
        if (failed !== null && current())
          write.error = `Management token created, but refreshing failed. ${errorMessage(failed)}`;
      },
      { key: 'create' }
    );
  }

  async function revoke(token: ManagementToken) {
    if (
      !(await confirmAction({
        title: `Revoke ${token.name}?`,
        body: 'Automation using this token loses access right away. You can’t undo this.',
        confirmLabel: 'Revoke token',
        danger: true
      }))
    )
      return;
    await write.run(
      () =>
        revokeIntent.submit(
          [token.id, token.etag] as const,
          (input, requestId) => revokeManagementToken(...input, requestId)
        ),
      async (_value, current) => {
        await tokens.refetch();
        if (current()) notify('Token revoked');
      },
      { key: `revoke-${token.id}` }
    );
  }

  async function copySecret() {
    if (!tokenSecret) return;
    const isCurrent = captureOwner();
    const didCopy = await copyText(tokenSecret.secret);
    if (!isCurrent()) return;
    if (!didCopy) {
      copied = false;
      copyError = 'Clipboard access is unavailable. Copy this token manually.';
      return;
    }
    copyError = '';
    copied = true;
  }
</script>

{#if tokenSecret}
  {@const created = tokenSecret}
  <SecretDialog
    eyebrow="Management token created"
    title="Copy the token secret now."
    description={`The secret is displayed once and expires at ${formatDate(created.expires_at)}. Store it somewhere safe; it cannot be recovered.`}
    onClose={() => {
      tokenSecret = null;
      copied = false;
      copyError = '';
    }}
  >
    {#snippet children(close)}
      <code class="token-secret">{created.secret}</code>
      {#if copyError}<div class="inline-problem" role="alert">
          {copyError}
        </div>{/if}
      <div class="dialog-actions">
        <button
          class="button button-secondary"
          type="button"
          onclick={copySecret}
        >
          {copied ? 'Token copied' : 'Copy token'}
        </button><button
          class="button button-primary"
          type="button"
          data-autofocus
          onclick={close}
        >
          I have saved it
        </button>
        <span class="sr-only" aria-live="polite">
          {copied ? 'Token secret copied to clipboard.' : ''}
        </span>
      </div>
    {/snippet}
  </SecretDialog>
{/if}

{#if write.error}<Callout tone="danger">{write.error}</Callout>{/if}

{#if !isOwner}
  <ReadOnlyNote>Only owners can manage management tokens.</ReadOnlyNote>
{:else}
  <div class="toolbar">
    <p class="lede">
      Tokens let scripts and CI call the management API with only the
      permissions you pick. Each secret is shown once.
    </p>
    <div class="page-actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => (creating = true)}
      >
        New token
      </button>
    </div>
  </div>
{/if}
{#if isOwner && creating}
  <Dialog
    title="New management token"
    placement="side"
    onClose={() => (creating = false)}
  >
    <ManagementTokenForm
      submitting={write.key === 'create'}
      {projects}
      onSubmit={create}
    />
  </Dialog>
{/if}

{#if isOwner}
  <QueryView
    query={tokens}
    loading="Loading management tokens…"
    errorTitle="Tokens could not be loaded"
    failure={errorMessage(tokens.error)}
    emptyWhen={!tokens.data?.items.length && pagination.history.length === 0}
  >
    {#snippet empty()}
      <EmptyState title="No management tokens yet">
        Create one when a script or pipeline needs to manage this installation.
      </EmptyState>
    {/snippet}
    <ScrollTable label="Management tokens">
      <table class="data-table">
        <thead>
          <tr>
            <th>Name / created by</th><th>Scopes</th><th>Projects</th><th>
              Status
            </th><th>Expires / created</th><th>
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {#each tokens.data?.items ?? [] as token (token.id)}
            <tr>
              <td>
                <strong>{token.name}</strong><br /><small>
                  Created by {token.created_by_email}
                </small>
              </td><td><code>{token.scopes.join(', ')}</code></td>
              <td>{projectScopeLabel(token)}</td>
              <td>
                <span
                  class:success={status(token) === 'active'}
                  class:warning={status(token) === 'expired'}
                  class:danger={status(token) === 'revoked'}
                  class="badge"
                >
                  {status(token)}
                </span>{#if token.revoked_at}<br /><small>
                    Revoked {formatDate(token.revoked_at)}
                  </small>{/if}
              </td>
              <td>
                {formatDate(token.expires_at)}<br /><small>
                  Created {formatDate(token.created_at)}
                </small>
              </td>
              <td>
                {#if status(token) === 'active'}<button
                    class="button button-secondary button-danger"
                    type="button"
                    onclick={() => revoke(token)}
                    disabled={write.busy}
                  >
                    Revoke
                  </button>{/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </ScrollTable>
    <CursorPagination
      {...cursorPaginationProps(pagination, tokens.data?.nextCursor)}
      label="Management token pages"
    />
  </QueryView>
{/if}

<style>
  .token-secret {
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
  td small {
    color: var(--foreground-muted);
  }
  @media (max-width: 42rem) {
    .dialog-actions {
      display: grid;
    }
  }
</style>
