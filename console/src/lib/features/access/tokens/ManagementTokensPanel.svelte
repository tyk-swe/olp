<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery } from '@tanstack/svelte-query';
  import { managementTokenKeys } from '#lib/features/access/tokens/tokenKeys.js';
  import {
    MANAGEMENT_TOKEN_SCOPES,
    createManagementToken,
    listManagementTokenPage,
    revokeManagementToken,
    type ManagementToken,
    type ManagementTokenScope,
    type ManagementTokenSecret
  } from '#lib/features/access/tokens/api.js';
  import { listProjects } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import { copyText } from '#lib/clipboard.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import SecretDialog from '#lib/shell/SecretDialog.svelte';
  import { formatDate } from '#lib/format.js';

  const revokeManagementTokenIntent = useMutationIntent();

  const createIntent = useMutationIntent();

  const expiryChoices = [
    { days: 7, label: '7 days' },
    { days: 30, label: '30 days' },
    { days: 90, label: '90 days' },
    { days: 180, label: '180 days' },
    // The server caps expiry at 366 days on its own clock; 365 leaves a
    // day of headroom for client clock skew.
    { days: 365, label: '365 days' }
  ];
  const scopeLabels: Record<string, string> = {
    read: 'Read management state',
    access_read: 'Read members and sessions',
    access: 'Manage members and provisioning',
    settings: 'Manage settings and pricing',
    configure: 'Manage providers and routes',
    keys: 'Manage API keys',
    manage_projects: 'Manage installation projects',
    manage_organization: 'Manage organizations',
    playground: 'Use the playground',
    usage: 'Read usage and request history'
  };

  function status(token: ManagementToken): string {
    if (token.revoked_at) return 'revoked';
    if (new Date(token.expires_at).getTime() <= Date.now()) return 'expired';
    return 'active';
  }

  const access = useRole();
  const isOwner = $derived(access.allows('POST /api/v1/management-tokens'));
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let creating = $state(false);
  let name = $state('');
  let scopes = $state<ManagementTokenScope[]>(['read']);
  let expiresInDays = $state(30);
  let allProjects = $state(true);
  let projectIds = $state<string[]>([]);
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
    name = '';
    scopes = ['read'];
    allProjects = true;
    projectIds = [];
    expiresInDays = 30;
    copied = false;
    copyError = error = busy = '';
    creating = false;
  });

  function toggle(scope: ManagementTokenScope) {
    scopes = scopes.includes(scope)
      ? scopes.filter((item) => item !== scope)
      : [...scopes, scope];
  }

  function toggleProject(id: string) {
    projectIds = projectIds.includes(id)
      ? projectIds.filter((item) => item !== id)
      : [...projectIds, id];
  }

  function projectScopeLabel(token: ManagementToken): string {
    if (token.all_projects) return 'All projects';
    const names = (token.project_ids ?? [])
      .map((id) => projectNames.get(id) ?? id)
      .join(', ');
    return names || 'No projects';
  }

  async function run(
    label: string,
    action: (isCurrent: () => boolean) => Promise<void>
  ) {
    const isCurrent = captureOwner();
    busy = label;
    error = '';
    try {
      await action(isCurrent);
    } catch (cause) {
      if (isCurrent()) error = errorMessage(cause);
    } finally {
      if (isCurrent()) busy = '';
    }
  }

  async function create(event: SubmitEvent) {
    event.preventDefault();
    if (!isOwner) return;
    if (!name.trim()) {
      error = 'Enter a token name.';
      return;
    }
    if (!scopes.length) {
      error = 'Select at least one scope.';
      return;
    }
    if (!allProjects && !projectIds.length) {
      error = 'Select at least one project, or choose all projects.';
      return;
    }
    await run('create', async (isCurrent) => {
      const submission = createIntent.prepare(
        {
          name: name.trim(),
          scopes,
          expiresInDays,
          projectIds: allProjects ? undefined : projectIds
        },
        () => ({
          name: name.trim(),
          scopes,
          expiresAt: new Date(
            Date.now() + expiresInDays * 24 * 60 * 60 * 1000
          ).toISOString(),
          projectIds: allProjects ? undefined : projectIds
        })
      );
      const result = await createManagementToken(
        submission.input.name,
        submission.input.scopes,
        submission.input.expiresAt,
        submission.input.projectIds,
        submission.requestId
      );
      if (!isCurrent()) return;
      tokenSecret = result;
      createIntent.reset();
      name = '';
      scopes = ['read'];
      allProjects = true;
      projectIds = [];
      creating = false;
      try {
        const observation = await tokens.refetch();
        if (observation?.isError) throw observation.error;
      } catch (cause) {
        if (isCurrent())
          error = `Management token created, but refreshing failed. ${errorMessage(cause)}`;
      }
    });
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
    await run(`revoke-${token.id}`, async (isCurrent) => {
      await revokeManagementTokenIntent.submit(
        [token.id, token.etag] as const,
        (input, requestId) => revokeManagementToken(...input, requestId)
      );
      if (!isCurrent()) return;
      await tokens.refetch();
      if (isCurrent()) notify('Token revoked');
    });
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
          onclick={copySecret}>{copied ? 'Token copied' : 'Copy token'}</button
        ><button
          class="button button-primary"
          type="button"
          data-autofocus
          onclick={close}>I have saved it</button
        >
        <span class="sr-only" aria-live="polite">
          {copied ? 'Token secret copied to clipboard.' : ''}
        </span>
      </div>
    {/snippet}
  </SecretDialog>
{/if}

{#if error}<Callout tone="danger">{error}</Callout>{/if}

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
        onclick={() => (creating = true)}>New token</button
      >
    </div>
  </div>
{/if}
{#if isOwner && creating}
  <Dialog
    title="New management token"
    placement="side"
    onClose={() => (creating = false)}
  >
    <form class="token-form" onsubmit={create} novalidate>
      <label
        ><span>Name</span><input
          type="text"
          autocomplete="off"
          bind:value={name}
          placeholder="deploy-automation"
          maxlength="100"
        /></label
      >
      <fieldset>
        <legend>Scopes</legend>
        {#each MANAGEMENT_TOKEN_SCOPES as scope (scope)}
          <label class="scope-option"
            ><input
              type="checkbox"
              checked={scopes.includes(scope)}
              onchange={() => toggle(scope)}
            />{scopeLabels[scope] ?? scope}<code>{scope}</code></label
          >
        {/each}
      </fieldset>
      <fieldset>
        <legend>Projects</legend>
        <label class="scope-option"
          ><input
            type="radio"
            name="token-projects"
            checked={allProjects}
            onchange={() => (allProjects = true)}
          />All projects</label
        >
        <label class="scope-option"
          ><input
            type="radio"
            name="token-projects"
            checked={!allProjects}
            onchange={() => (allProjects = false)}
          />Selected projects</label
        >
        {#if !allProjects}
          {#if projects.isPending}<span class="inline-status" role="status"
              >Loading projects…</span
            >{:else if projects.isError}<span
              class="inline-problem"
              role="alert"
              >Projects are unavailable.
              <button
                class="text-button"
                type="button"
                onclick={() => projects.refetch()}>Retry</button
              ></span
            >{:else}{#each projects.data ?? [] as project (project.id)}<label
                class="scope-option"
                ><input
                  type="checkbox"
                  checked={projectIds.includes(project.id)}
                  onchange={() => toggleProject(project.id)}
                />{project.name}</label
              >{:else}<span
                >No projects yet. Create one under Access, Projects.</span
              >{/each}{/if}
        {/if}
      </fieldset>
      <label
        ><span>Expires in</span><select bind:value={expiresInDays}
          >{#each expiryChoices as choice (choice.days)}<option
              value={choice.days}>{choice.label}</option
            >{/each}</select
        ></label
      >
      <button
        class="button button-primary"
        type="submit"
        disabled={busy === 'create'}
        >{busy === 'create' ? 'Creating…' : 'Create token'}</button
      >
    </form>
  </Dialog>
{/if}

{#if isOwner}
  {#if tokens.isPending}
    <div class="loading-state" role="status">Loading management tokens…</div>
  {:else if tokens.isError}
    <Callout tone="danger" title="Tokens could not be loaded"
      >{errorMessage(tokens.error)}</Callout
    >
  {:else if !tokens.data?.items.length && pagination.history.length === 0}
    <EmptyState title="No management tokens yet"
      >Create one when a script or pipeline needs to manage this installation.</EmptyState
    >
  {:else}
    <div class="table-shell">
      <table class="data-table">
        <thead
          ><tr
            ><th>Name / created by</th><th>Scopes</th><th>Projects</th><th
              >Status</th
            ><th>Expires / created</th><th
              ><span class="sr-only">Actions</span></th
            ></tr
          ></thead
        >
        <tbody>
          {#each tokens.data?.items ?? [] as token (token.id)}
            <tr>
              <td
                ><strong>{token.name}</strong><br /><small
                  >Created by {token.created_by_email}</small
                ></td
              ><td><code>{token.scopes.join(', ')}</code></td>
              <td>{projectScopeLabel(token)}</td>
              <td
                ><span
                  class:success={status(token) === 'active'}
                  class:warning={status(token) === 'expired'}
                  class:danger={status(token) === 'revoked'}
                  class="badge">{status(token)}</span
                >{#if token.revoked_at}<br /><small
                    >Revoked {formatDate(token.revoked_at)}</small
                  >{/if}</td
              >
              <td
                >{formatDate(token.expires_at)}<br /><small
                  >Created {formatDate(token.created_at)}</small
                ></td
              >
              <td
                >{#if status(token) === 'active'}<button
                    class="button button-secondary button-danger"
                    type="button"
                    onclick={() => revoke(token)}
                    disabled={Boolean(busy)}>Revoke</button
                  >{/if}</td
              >
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(pagination, tokens.data?.nextCursor)}
      label="Management token pages"
    />
  {/if}
{/if}

<style>
  .lede {
    max-width: 60ch;
    margin: 0;
    color: var(--foreground-muted);
  }
  .token-form {
    display: grid;
    gap: 0.65rem;
  }
  .token-form label {
    display: grid;
    gap: 0.4rem;
    font-weight: 500;
  }
  .token-form input[type='text'],
  .token-form select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .token-form input:hover,
  .token-form select:hover {
    border-color: var(--border-strong);
  }
  .token-form input::placeholder {
    color: var(--foreground-muted);
  }
  .token-form fieldset {
    display: grid;
    gap: 0.4rem;
    margin: 0;
    padding: 0.65rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
  }
  .token-form legend {
    padding: 0 0.3rem;
    font-weight: 500;
  }
  .scope-option {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 400;
  }
  .scope-option code {
    margin-left: auto;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
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
  @media (max-width: 64rem) {
    .token-form {
      display: grid;
    }
  }
  @media (max-width: 42rem) {
    .dialog-actions {
      display: grid;
    }
  }
</style>
