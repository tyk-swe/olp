<script lang="ts">
  import { focusFormError, focusErrorSummary } from '$lib/forms/focusError';
  import { routeKeys } from '$lib/features/routes/routeKeys';

  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import type { ApiKey } from '$lib/features/access/api-keys/api';
  import { listRoutes } from '$lib/features/routes/api';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatBudget, formatDate, formatInteger } from '$lib/format';
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import { validateApiKey } from '$lib/features/access/api-keys/keyValidation';
  import {
    buildApiKeyPolicyInput,
    createApiKeyFormState,
    type ApiKeyPolicyInput
  } from '$lib/features/access/api-keys/apiKeyPolicy';

  let {
    editing,
    busy,
    submitError,
    canManage,
    publicationBlocked = false,
    onSubmit,
    onCancel,
    onClearError
  }: {
    editing: ApiKey | null;
    busy: string;
    submitError: string;
    canManage: boolean;
    publicationBlocked?: boolean;
    onSubmit: (
      input: ApiKeyPolicyInput,
      preferredRoute?: string
    ) => boolean | Promise<boolean>;
    onCancel: () => void;
    onClearError: () => void;
  } = $props();

  let form = $state(createApiKeyFormState());
  let errors = $state<Record<string, string>>({});
  let formError = $state('');
  let dirty = $state(false);
  let initialized = $state(false);
  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
  }));

  $effect(() => {
    if (initialized) return;
    initialized = true;
    form = createApiKeyFormState(editing);
  });

  guardUnsavedChanges(() => dirty);

  function touch() {
    dirty = true;
  }

  function toggle(list: string[], value: string, checked: boolean) {
    return checked
      ? [...new Set([...list, value])]
      : list.filter((item) => item !== value);
  }

  // Keep numeric controls as strings via oninput: bind:value on type="number"
  // coerces zero to a number, which would be mistaken for an omitted limit.
  function numberValue(value: string) {
    return value ? Number(value) : undefined;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const root =
      (event.currentTarget as HTMLFormElement).closest('main') ??
      (event.currentTarget as HTMLFormElement);
    if (!canManage || busy || publicationBlocked) return;
    onClearError();
    formError = '';
    errors = validateApiKey({
      name: form.name,
      requestsPerMinute: numberValue(form.requestsPerMinute),
      tokensPerMinute: numberValue(form.tokensPerMinute),
      maxConcurrency: numberValue(form.maxConcurrency),
      dailyCostLimit: form.dailyCostLimit,
      monthlyCostLimit: form.monthlyCostLimit,
      expiresAt: form.expiresAt
    });
    if (Object.keys(errors).length) {
      await focusFormError(root);
      return;
    }
    if (!form.scopes.length) {
      formError = 'Select at least one scope.';
      await focusFormError(root);
      return;
    }
    const saved = await onSubmit(
      buildApiKeyPolicyInput(form),
      form.allowedRoutes[0]
    );
    if (saved) dirty = false;
    else await focusFormError(root);
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Access · API Keys</p>
    <h1 class="page-title">
      {editing
        ? canManage
          ? 'Edit key policy.'
          : 'View key policy.'
        : 'Create a proxy key.'}
    </h1>
    <p class="page-description">
      {editing
        ? 'Update scopes, route access, expiry, and shared hard limits. The key secret does not change.'
        : 'Scope access, restrict route slugs, and apply shared hard limits. The secret is displayed once.'}
    </p>
  </div>
  {#if editing}<button
      class="button button-secondary"
      type="button"
      disabled={Boolean(busy)}
      onclick={onCancel}>Cancel</button
    >{:else}<a class="button button-secondary" href={resolve('/api-keys')}
      >Cancel</a
    >{/if}
</div>
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
    <p class="eyebrow">Identity</p>
    <h2 id="identity-heading">Name and expiration</h2>
    <div class="form-grid">
      <div class="form-field">
        <label for="key-name">Key name</label><input
          id="key-name"
          bind:value={form.name}
          disabled={!canManage}
          aria-invalid={errors.name ? 'true' : undefined}
          aria-describedby={errors.name ? 'key-name-error' : undefined}
        />{#if errors.name}<small class="field-error" id="key-name-error"
            >{errors.name}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="key-expiry">Expires at (optional)</label><input
          id="key-expiry"
          type="datetime-local"
          bind:value={form.expiresAt}
          disabled={!canManage}
          aria-invalid={errors.expiresAt ? 'true' : undefined}
          aria-describedby={errors.expiresAt ? 'key-expiry-error' : undefined}
        />{#if errors.expiresAt}<small class="field-error" id="key-expiry-error"
            >{errors.expiresAt}</small
          >{/if}
      </div>
    </div>
  </section>
  <section aria-labelledby="scope-heading">
    <p class="eyebrow">Authorization</p>
    <h2 id="scope-heading">Scopes and route allowlist</h2>
    <fieldset class="checks">
      <legend>Scopes</legend
      >{#each [['inference', 'Inference requests'], ['models_read', 'Model listing']] as scope (scope[0])}<label
          ><input
            type="checkbox"
            checked={form.scopes.includes(scope[0])}
            disabled={!canManage}
            onchange={(event) =>
              (form.scopes = toggle(
                form.scopes,
                scope[0],
                event.currentTarget.checked
              ))}
          />
          {scope[1]}</label
        >{/each}
    </fieldset>
    <fieldset class="checks routes">
      <legend>Allowed route slugs</legend>
      <p>Leave every route unchecked to allow all current and future routes.</p>
      {#if routes.isPending}<span role="status">Loading routes…</span
        >{:else if routes.isError}<span class="inline-problem" role="alert"
          >Routes are unavailable, so route restrictions cannot be reviewed.
          <button
            class="text-button"
            type="button"
            onclick={() => routes.refetch()}>Retry</button
          ></span
        >{:else}{#each routes.data ?? [] as route (route.id)}<label
            ><input
              type="checkbox"
              checked={form.allowedRoutes.includes(route.slug)}
              disabled={!canManage}
              onchange={(event) =>
                (form.allowedRoutes = toggle(
                  form.allowedRoutes,
                  route.slug,
                  event.currentTarget.checked
                ))}
            /> <code>{route.slug}</code></label
          >{/each}{#if !routes.data?.length}<span
            >No routes are configured yet.</span
          >{/if}{/if}
    </fieldset>
  </section>
  <section aria-labelledby="limits-heading">
    <p class="eyebrow">Distributed limits</p>
    <h2 id="limits-heading">Hard runtime limits</h2>
    <p class="section-help">
      These limits follow the installation's Valkey outage policy. Leave blank
      for no limit.
    </p>
    <div class="form-grid limits">
      <div class="form-field">
        <label for="rpm">Requests per minute</label><input
          id="rpm"
          type="number"
          min="1"
          inputmode="numeric"
          value={form.requestsPerMinute}
          oninput={(event) =>
            (form.requestsPerMinute = event.currentTarget.value)}
          disabled={!canManage}
          aria-invalid={errors.requestsPerMinute ? 'true' : undefined}
          aria-describedby={errors.requestsPerMinute ? 'rpm-error' : undefined}
        />{#if errors.requestsPerMinute}<small
            class="field-error"
            id="rpm-error">{errors.requestsPerMinute}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="tpm">Tokens per minute</label><input
          id="tpm"
          type="number"
          min="1"
          inputmode="numeric"
          value={form.tokensPerMinute}
          oninput={(event) =>
            (form.tokensPerMinute = event.currentTarget.value)}
          disabled={!canManage}
          aria-invalid={errors.tokensPerMinute ? 'true' : undefined}
          aria-describedby={errors.tokensPerMinute ? 'tpm-error' : undefined}
        />{#if errors.tokensPerMinute}<small class="field-error" id="tpm-error"
            >{errors.tokensPerMinute}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="concurrency">Concurrent requests</label><input
          id="concurrency"
          type="number"
          min="1"
          inputmode="numeric"
          value={form.maxConcurrency}
          oninput={(event) => (form.maxConcurrency = event.currentTarget.value)}
          disabled={!canManage}
          aria-invalid={errors.maxConcurrency ? 'true' : undefined}
          aria-describedby={errors.maxConcurrency
            ? 'concurrency-error'
            : undefined}
        />{#if errors.maxConcurrency}<small
            class="field-error"
            id="concurrency-error">{errors.maxConcurrency}</small
          >{/if}
      </div>
    </div>
  </section>
  <section aria-labelledby="budget-heading">
    <p class="eyebrow">Spend controls</p>
    <h2 id="budget-heading">Cost budgets</h2>
    <p class="section-help">
      Amounts use the installation pricing currency. Daily and monthly windows
      reset at midnight UTC. Budgeted requests are refused when Valkey is
      unavailable. These are accrued-cost thresholds: accepted concurrent work
      can exceed them, and unpriced attempts accrue no cost. Leave blank for no
      cost budget.
    </p>
    <div class="form-grid budget-inputs">
      <div class="form-field">
        <label for="daily-budget">Daily cost budget (optional)</label><input
          id="daily-budget"
          inputmode="decimal"
          placeholder="10.00"
          bind:value={form.dailyCostLimit}
          disabled={!canManage}
          aria-invalid={errors.dailyCostLimit ? 'true' : undefined}
          aria-describedby={errors.dailyCostLimit
            ? 'daily-budget-error'
            : undefined}
        />{#if errors.dailyCostLimit}<small
            class="field-error"
            id="daily-budget-error">{errors.dailyCostLimit}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="monthly-budget">Monthly cost budget (optional)</label><input
          id="monthly-budget"
          inputmode="decimal"
          placeholder="100.00"
          bind:value={form.monthlyCostLimit}
          disabled={!canManage}
          aria-invalid={errors.monthlyCostLimit ? 'true' : undefined}
          aria-describedby={errors.monthlyCostLimit
            ? 'monthly-budget-error'
            : undefined}
        />{#if errors.monthlyCostLimit}<small
            class="field-error"
            id="monthly-budget-error">{errors.monthlyCostLimit}</small
          >{/if}
      </div>
    </div>
    {#if editing}
      <div
        class="budget-detail"
        role="region"
        aria-label="Current spend budget"
      >
        <div>
          <span>Daily accrued / limit</span>
          <strong
            >{formatBudget(editing.budget.daily.accrued)} / {editing.budget
              .daily.limit === null
              ? 'No limit'
              : formatBudget(editing.budget.daily.limit)}</strong
          >
          <small
            >Window ends (local time) {formatDate(
              editing.budget.daily.window_ends_at
            )}</small
          >
        </div>
        <div>
          <span>Monthly accrued / limit</span>
          <strong
            >{formatBudget(editing.budget.monthly.accrued)} / {editing.budget
              .monthly.limit === null
              ? 'No limit'
              : formatBudget(editing.budget.monthly.limit)}</strong
          >
          <small
            >Window ends (local time) {formatDate(
              editing.budget.monthly.window_ends_at
            )}</small
          >
        </div>
        <div>
          <span>Unpriced attempts this UTC month</span>
          <strong>{formatInteger(editing.budget.unpriced_attempts)}</strong>
          <small>Unpriced attempts accrue 0 toward both budgets.</small>
        </div>
      </div>
    {/if}
  </section>
  {#if publicationBlocked}<p class="section-help" role="status">
      Save the routing policy before publishing this key.
    </p>{/if}
  {#if canManage || !editing}<div class="form-actions">
      <button
        class="button button-primary"
        type="submit"
        disabled={!canManage || Boolean(busy) || publicationBlocked}
        >{busy === 'create'
          ? 'Creating securely…'
          : busy === 'update'
            ? 'Publishing policy…'
            : editing
              ? 'Save and publish'
              : 'Create and show key'}
        <NavIcon name="arrow" /></button
      >
    </div>{/if}
</form>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
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
  .key-form section + section {
    padding-top: 1.5rem;
    border-top: 1px solid var(--border-hairline);
  }
  .checks {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem 1rem;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .checks legend {
    width: 100%;
    margin-bottom: 0.4rem;
    font-weight: 500;
  }
  .checks label {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
  }
  .checks.routes {
    display: grid;
    margin-top: 1rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .checks.routes p,
  .section-help {
    margin: 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  .limits {
    grid-template-columns: repeat(3, 1fr);
  }
  .budget-inputs {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .budget-detail {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.75rem;
    margin-top: 1rem;
  }
  .budget-detail div {
    display: grid;
    gap: 0.35rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .budget-detail span {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .budget-detail strong {
    font-size: 1.25rem;
    font-weight: 400;
    font-variant-numeric: tabular-nums;
    letter-spacing: -0.02em;
    line-height: 1.2;
  }
  .budget-detail small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .field-error {
    color: var(--danger);
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
  }
  code {
    font-size: var(--text-caption);
  }
  @media (max-width: 48rem) {
    .limits,
    .budget-inputs,
    .budget-detail {
      grid-template-columns: 1fr;
    }
  }
</style>
