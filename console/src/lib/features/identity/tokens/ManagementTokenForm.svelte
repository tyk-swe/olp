<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import { focusFormError } from '#lib/editing/focusError.js';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import type { ManagementTokenScope } from '#lib/features/identity/tokens/api.js';
  import type { Project } from '#lib/domain/projects/api.js';
  import {
    newTokenDraft,
    type TokenDraft,
    type TokenField
  } from './tokenDraft';

  let {
    submitting,
    error,
    errors,
    projects,
    onSubmit
  }: {
    submitting: boolean;
    error: string;
    errors: FieldErrors<TokenField>;
    projects: {
      isPending: boolean;
      isError: boolean;
      data?: Project[];
      refetch: () => Promise<unknown>;
    };
    onSubmit: (draft: TokenDraft) => Promise<boolean>;
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
  const scopeLabels: Record<ManagementTokenScope, string> = {
    read: 'Read management state',
    access_read: 'Read members and sessions',
    access: 'Manage members and provisioning',
    settings: 'Manage settings and pricing',
    configure: 'Manage providers and routes',
    keys: 'Manage API keys',
    manage_organization: 'Manage organizations',
    manage_projects: 'Manage installation projects',
    playground: 'Use the playground',
    usage: 'Read usage and request history'
  };
  const scopes = Object.keys(scopeLabels) as ManagementTokenScope[];

  const id = $props.id();
  let draft = $state(newTokenDraft());

  usePrincipalLifetime(() => {
    draft = newTokenDraft();
  });

  function reported(field: 'scopes' | 'projectIds') {
    return errors[field]
      ? {
          'aria-invalid': 'true' as const,
          'aria-describedby': `${id}-${field}`
        }
      : {};
  }

  function toggle(scope: ManagementTokenScope) {
    draft.scopes = draft.scopes.includes(scope)
      ? draft.scopes.filter((item) => item !== scope)
      : [...draft.scopes, scope];
  }

  function toggleProject(projectId: string) {
    draft.projectIds = draft.projectIds.includes(projectId)
      ? draft.projectIds.filter((item) => item !== projectId)
      : [...draft.projectIds, projectId];
  }

  async function create(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    if (await onSubmit(draft)) draft = newTokenDraft();
    else await focusFormError(form);
  }
</script>

{#snippet problemAt(field: 'scopes' | 'projectIds')}
  {#if errors[field]}<p class="field-error" id="{id}-{field}">
      {errors[field]}
    </p>{/if}
{/snippet}

<form class="token-form" onsubmit={create} novalidate>
  <FormField label="Name" error={errors.name}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="text"
        autocomplete="off"
        bind:value={draft.name}
        placeholder="deploy-automation"
        maxlength="100"
      />
    {/snippet}
  </FormField>
  <fieldset>
    <legend>Scopes</legend>
    {#each scopes as scope (scope)}
      <label class="scope-option">
        <input
          type="checkbox"
          checked={draft.scopes.includes(scope)}
          onchange={() => toggle(scope)}
          {...reported('scopes')}
        />{scopeLabels[scope]}<code>{scope}</code>
      </label>
    {/each}
    {@render problemAt('scopes')}
  </fieldset>
  <fieldset>
    <legend>Projects</legend>
    <label class="scope-option">
      <input
        type="radio"
        name="token-projects"
        checked={draft.allProjects}
        onchange={() => (draft.allProjects = true)}
      />All projects
    </label>
    <label class="scope-option">
      <input
        type="radio"
        name="token-projects"
        checked={!draft.allProjects}
        onchange={() => (draft.allProjects = false)}
        {...reported('projectIds')}
      />Selected projects
    </label>
    {#if !draft.allProjects}
      {#if projects.isPending}<span class="inline-status" role="status">
          Loading projects…
        </span>{:else if projects.isError}<span
          class="inline-problem"
          role="alert"
        >
          Projects are unavailable.
          <button
            class="text-button"
            type="button"
            onclick={() => projects.refetch()}
          >
            Retry
          </button>
        </span>{:else}{#each projects.data ?? [] as project (project.id)}<label
            class="scope-option"
          >
            <input
              type="checkbox"
              checked={draft.projectIds.includes(project.id)}
              onchange={() => toggleProject(project.id)}
            />{project.name}
          </label>{:else}<span>
            No projects yet. Create one under Access, Projects.
          </span>{/each}{/if}
    {/if}
    {@render problemAt('projectIds')}
  </fieldset>
  <FormField label="Expires in" error={errors.expiresInDays}>
    {#snippet control(attributes)}
      <select {...attributes} bind:value={draft.expiresInDays}>
        {#each expiryChoices as choice (choice.days)}<option
            value={choice.days}
          >
            {choice.label}
          </option>{/each}
      </select>
    {/snippet}
  </FormField>
  {#if error}<p
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
    >
      {error}
    </p>{/if}
  <button class="button button-primary" type="submit" disabled={submitting}>
    {submitting ? 'Creating…' : 'Create token'}
  </button>
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
