<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { managementTokenKeys } from '$lib/features/access/tokens/tokenKeys';
  import {
    MANAGEMENT_TOKEN_SCOPES,
    createManagementToken,
    listManagementTokenPage,
    revokeManagementToken,
    listProjectPage,
    type ManagementToken,
    type ManagementTokenScope,
    type ManagementTokenSecret
  } from '$lib/features/access/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { copyText } from '$lib/clipboard';
  import { errorMessage } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import { formatDate } from '$lib/format';

  const expiryChoices = [
    { days: 7, label: '7 days' },
    { days: 30, label: '30 days' },
    { days: 90, label: '90 days' },
    { days: 180, label: '180 days' },
    { days: 366, label: '366 days' }
  ];
  const scopeLabels: Record<string, string> = {
    read: 'Read management state',
    access_read: 'Read members and sessions',
    access: 'Manage members and provisioning',
    settings: 'Manage settings and pricing',
    configure: 'Manage providers and routes',
    keys: 'Manage API keys',
    playground: 'Use the playground',
    usage: 'Read usage and request history'
  };

  function status(token: ManagementToken): string {
    if (token.revoked_at) return 'revoked';
    if (new Date(token.expires_at).getTime() <= Date.now()) return 'expired';
    return 'active';
  }

  const access = useRole();
  const isOwner = $derived(access.role === 'owner');
  const pagination = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
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
    queryKey: projectKeys.page(),
    queryFn: ({ signal }) => listProjectPage(undefined, signal),
    enabled: isOwner
  }));
  const projectNames = $derived(
    new Map((projects.data?.items ?? []).map((item) => [item.id, item.name]))
  );

  onDestroy(() => {
    tokenSecret = null;
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
    await run('create', async () => {
      const expiresAt = new Date(
        Date.now() + expiresInDays * 24 * 60 * 60 * 1000
      ).toISOString();
      tokenSecret = await createManagementToken(
        name.trim(),
        scopes,
        expiresAt,
        allProjects ? undefined : projectIds
      );
      name = '';
      scopes = ['read'];
      allProjects = true;
      projectIds = [];
      await tokens.refetch();
    });
  }

  async function revoke(token: ManagementToken) {
    if (!confirm(`Revoke the management token "${token.name}"?`)) return;
    await run(`revoke-${token.id}`, async () => {
      await revokeManagementToken(token.id, token.etag);
      await tokens.refetch();
      notice = 'Management token revoked.';
    });
  }

  async function copySecret() {
    if (!tokenSecret) return;
    if (!(await copyText(tokenSecret.secret))) {
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

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if !isOwner}
  <ReadOnlyNote>Only owners can manage management tokens.</ReadOnlyNote>
{:else}
  <section class="card token-panel" aria-labelledby="token-create-heading">
    <div>
      <p class="eyebrow">New management token</p>
      <h2 id="token-create-heading">Scoped machine credential</h2>
      <p>
        The secret is shown once. Tokens authorize exactly the selected
        management operations until expiry or revocation.
      </p>
    </div>
    <form onsubmit={create} novalidate>
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
          {#if projects.isPending}<span role="status">Loading projects…</span
            >{:else if projects.isError}<span
              class="inline-problem"
              role="alert"
              >Projects are unavailable.
              <button
                class="text-button"
                type="button"
                onclick={() => projects.refetch()}>Retry</button
              ></span
            >{:else}{#each projects.data?.items ?? [] as project (project.id)}<label
                class="scope-option"
                ><input
                  type="checkbox"
                  checked={projectIds.includes(project.id)}
                  onchange={() => toggleProject(project.id)}
                />{project.name}</label
              >{:else}<span
                >No projects exist. Create one under the Projects tab.</span
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
  </section>
{/if}

{#if isOwner}
  {#if tokens.isPending}
    <div class="loading-state" role="status">Loading management tokens…</div>
  {:else if tokens.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(tokens.error)}
    </div>
  {:else if !tokens.data?.items.length && pagination.history.length === 0}
    <section class="card empty-state">
      <p>No management tokens have been created.</p>
    </section>
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
                    class="button button-secondary danger-button"
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
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .token-panel {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 2rem;
    margin-bottom: 1rem;
    padding: 1.5rem;
  }
  .token-panel h2 + p {
    margin: 0.4rem 0 0;
    color: var(--foreground-muted);
  }
  .token-panel form {
    display: grid;
    gap: 0.65rem;
  }
  .token-panel label {
    display: grid;
    gap: 0.4rem;
    font-weight: 500;
  }
  .token-panel input[type='text'],
  .token-panel select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .token-panel input:hover,
  .token-panel select:hover {
    border-color: var(--border-strong);
  }
  .token-panel input::placeholder {
    color: var(--foreground-muted);
  }
  .token-panel fieldset {
    display: grid;
    gap: 0.4rem;
    margin: 0;
    padding: 0.65rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
  }
  .token-panel legend {
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
  .danger-button {
    color: var(--danger);
  }
  td small {
    color: var(--foreground-muted);
  }
  @media (max-width: 64rem) {
    .token-panel {
      display: grid;
    }
  }
  @media (max-width: 42rem) {
    .dialog-actions {
      display: grid;
    }
  }
</style>
