<script lang="ts">
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  const services = useServiceCapabilities();
  import {
    focusFormError,
    focusErrorSummary
  } from '#lib/editing/focusError.js';
  import { routeQueries } from '#lib/domain/routes/queries.js';

  import { createQuery } from '@tanstack/svelte-query';
  import type { ApiKey } from '#lib/features/access/api-keys/api.js';
  import ApiKeyOverlaps from './ApiKeyOverlaps.svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';

  import {
    compileApiKeyPolicy,
    createApiKeyFormState,
    type ApiKeyPolicyCommand
  } from '#lib/features/access/api-keys/apiKeyPolicy.js';
  import ApiKeyScopeFields from './ApiKeyScopeFields.svelte';
  import ApiKeyRateFields from './ApiKeyRateFields.svelte';
  import ApiKeySpendingFields from './ApiKeySpendingFields.svelte';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import {
    listBudgetGroups,
    budgetGroupKeys
  } from '#lib/features/budgets/index.js';

  let {
    editing,
    overlaps,
    busy,
    submitError,
    canManage: permissionToManage,
    publicationBlocked = false,
    onSubmit,
    onSaved,
    onCancel,
    onClearError
  }: {
    editing: ApiKey | null;
    overlaps: ApiKey['active_overlaps'];
    busy: string;
    submitError: string;
    canManage: boolean;
    publicationBlocked?: boolean;
    onSubmit: (
      command: ApiKeyPolicyCommand,
      preferredRoute?: string
    ) => boolean | Promise<boolean>;
    onSaved: () => void;
    onCancel: () => void;
    onClearError: () => void;
  } = $props();

  const canManage = $derived(
    permissionToManage && !editing?.workload_issuer_id
  );

  let form = $state(createApiKeyFormState());
  let errors = $state<Record<string, string>>({});
  let formError = $state('');
  let dirty = $state(false);
  let editRevision = 0;
  let initialized = $state(false);
  const routes = createQuery(() => ({
    ...routeQueries.all(),
    enabled: services.gatewayAvailable
  }));
  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));

  const formProject = $derived(
    editing ? (editing.project_id ?? null) : form.projectId || null
  );
  const matchingGroups = $derived(
    (groups.data ?? []).filter((group) => group.project_id === formProject)
  );

  $effect(() => {
    if (
      form.budgetGroupId &&
      groups.data &&
      !matchingGroups.some((group) => group.id === form.budgetGroupId)
    ) {
      form.budgetGroupId = '';
    }
  });

  $effect(() => {
    if (initialized) return;
    initialized = true;
    form = createApiKeyFormState(editing);
  });

  guardUnsavedChanges(() => dirty);

  function touch() {
    editRevision++;
    dirty = true;
  }

  function toggle(list: string[], value: string, checked: boolean) {
    return checked
      ? [...new Set([...list, value])]
      : list.filter((item) => item !== value);
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();

    const root =
      (event.currentTarget as HTMLFormElement).closest('main') ??
      (event.currentTarget as HTMLFormElement);

    if (!canManage || busy || publicationBlocked) return;
    onClearError();

    const result = compileApiKeyPolicy(
      form,
      editing ? { kind: 'update', key: editing } : { kind: 'create' },
      new Date()
    );
    if (!result.ok) {
      errors = result.errors;
      formError = result.formError;
      await focusFormError(root);
      return;
    }
    errors = {};
    formError = '';
    const submittedRevision = editRevision;
    const saved = await onSubmit(result.command, form.allowedRoutes[0]);
    if (saved && editRevision === submittedRevision) {
      dirty = false;
      onSaved();
    } else if (!saved) await focusFormError(root);
  }
</script>

<PageHeader
  title={editing ? (canManage ? 'Edit key' : 'Key details') : 'New API key'}
  description={!services.limitsEnforced
    ? 'Limits and budgets you set here are saved but not enforced on this installation.'
    : editing
      ? 'Changes apply to the existing secret. Apps using it don’t need updating.'
      : 'You’ll see the secret once, right after you create the key.'}
/>
{#if submitError || formError}<div
    class="inline-problem"
    role="alert"
    tabindex="-1"
    data-error-summary
    use:focusErrorSummary
  >
    {submitError || formError}
  </div>{/if}
{#if !canManage}
  <ReadOnlyNote>
    {editing
      ? 'This API key policy can be viewed but not changed.'
      : 'Your role can view API key policies but not create or change them.'}
  </ReadOnlyNote>
{/if}

<p class="sr-only" role="status">{busy ? 'Saving key policy…' : ''}</p>
<form
  class="card key-form"
  aria-busy={Boolean(busy)}
  onsubmit={submit}
  oninput={touch}
  onchange={touch}
  novalidate
>
  <section aria-labelledby="identity-heading">
    <h2 id="identity-heading">Basics</h2>
    <div class="form-grid">
      <div class="form-field">
        <label for="key-name">Key name</label><input
          id="key-name"
          bind:value={form.name}
          disabled={!canManage}
          aria-invalid={errors.name ? 'true' : undefined}
          aria-describedby={errors.name ? 'key-name-error' : undefined}
        />{#if errors.name}<small class="field-error" id="key-name-error">
            {errors.name}
          </small>{/if}
      </div>
      <div class="form-field">
        <label for="key-expiry">Expires at (optional)</label><input
          id="key-expiry"
          type="datetime-local"
          bind:value={form.expiresAt}
          disabled={!canManage}
          aria-invalid={errors.expiresAt ? 'true' : undefined}
          aria-describedby={errors.expiresAt ? 'key-expiry-error' : undefined}
        />{#if errors.expiresAt}<small
            class="field-error"
            id="key-expiry-error"
          >
            {errors.expiresAt}
          </small>{/if}
      </div>
      {#if !editing}<ProjectScopeField
          id="key-project"
          bind:value={form.projectId}
          disabled={!canManage}
        />{:else}<div class="form-field">
          <span class="scope-label">Project</span><span>
            {editing.project_name ?? 'Installation-wide'}
          </span>
        </div>{/if}
    </div>
  </section>
  <ApiKeyOverlaps overlaps={overlaps ?? []} />
  <ApiKeyScopeFields
    {form}
    {errors}
    {canManage}
    {busy}
    {publicationBlocked}
    {services}
    {routes}
    {toggle}
    {touch}
  />
  <ApiKeyRateFields {form} {errors} {canManage} {services} />
  <ApiKeySpendingFields
    {form}
    {errors}
    {canManage}
    {editing}
    {services}
    {groups}
    {matchingGroups}
  />
  {#if publicationBlocked}<p class="section-help" role="status">
      Save the routing policy before publishing this key.
    </p>{/if}
  {#if canManage || !editing}<div class="form-actions">
      <button
        class="button button-secondary"
        type="button"
        disabled={Boolean(busy)}
        onclick={onCancel}
      >
        Cancel
      </button>
      <button
        class="button button-primary"
        type="submit"
        disabled={!canManage || Boolean(busy) || publicationBlocked}
      >
        {busy === 'create'
          ? 'Creating securely…'
          : busy === 'update'
            ? 'Saving policy…'
            : editing
              ? services.gatewayAvailable
                ? 'Save and publish'
                : 'Save policy'
              : 'Create and show key'}
        <NavIcon name="arrow" />
      </button>
    </div>{/if}
</form>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .key-form {
    display: grid;
    max-width: 66rem;
    gap: 2rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .scope-label {
    display: block;
    margin-bottom: 0.3rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
  }
</style>
