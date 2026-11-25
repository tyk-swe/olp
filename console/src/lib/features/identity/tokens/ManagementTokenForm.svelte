<script lang="ts">
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import {
    MANAGEMENT_TOKEN_SCOPES,
    type ManagementTokenScope
  } from '#lib/features/identity/tokens/api.js';
  import type { listProjects } from '#lib/features/access/index.js';

  export type TokenFormInput = {
    name: string;
    scopes: ManagementTokenScope[];
    expiresInDays: number;
    allProjects: boolean;
    projectIds: string[];
  };

  let {
    submitting,
    projects,
    onSubmit
  }: {
    submitting: boolean;
    projects: {
      isPending: boolean;
      isError: boolean;
      data?: Awaited<ReturnType<typeof listProjects>>;
      refetch: () => Promise<unknown>;
    };
    onSubmit: (input: TokenFormInput) => Promise<boolean>;
  } = $props();

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

  let name = $state('');
  let scopes = $state<ManagementTokenScope[]>(['read']);
  let expiresInDays = $state(30);
  let allProjects = $state(true);
  let projectIds = $state<string[]>([]);

  usePrincipalLifetime(() => {
    name = '';
    scopes = ['read'];
    allProjects = true;
    projectIds = [];
    expiresInDays = 30;
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

  async function create(event: SubmitEvent) {
    event.preventDefault();
    if (
      await onSubmit({ name, scopes, expiresInDays, allProjects, projectIds })
    ) {
      name = '';
      scopes = ['read'];
      allProjects = true;
      projectIds = [];
    }
  }
</script>

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
        >{:else if projects.isError}<span class="inline-problem" role="alert"
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
      >{#each expiryChoices as choice (choice.days)}<option value={choice.days}
          >{choice.label}</option
        >{/each}</select
    ></label
  >
  <button class="button button-primary" type="submit" disabled={submitting}
    >{submitting ? 'Creating…' : 'Create token'}</button
  >
</form>

<style>
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
  @media (max-width: 64rem) {
    .token-form {
      display: grid;
    }
  }
</style>
