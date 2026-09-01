<script lang="ts">
  import { resolve } from '$app/paths';
  import { queryKeys } from '$lib/api/queryKeys';
  import { createQuery } from '@tanstack/svelte-query';
  import type { ApiKey } from '$lib/api/management/api-keys';
  import { listRoutes } from '$lib/api/management/routes';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatCost, formatDate, formatInteger } from '$lib/format';
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import { validateApiKey } from './keyValidation';
  import {
    buildApiKeyPolicyInput,
    createApiKeyFormState,
    type ApiKeyPolicyInput
  } from './apiKeyPolicy';

  let {
    editing,
    busy,
    submitError,
    canManage,
    onSubmit,
    onCancel,
    onClearError
  }: {
    editing: ApiKey | null;
    busy: string;
    submitError: string;
    canManage: boolean;
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
    queryKey: queryKeys.routes.all(),
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

  function numberValue(value: string) {
    return value ? Number(value) : undefined;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage) return;
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
    if (Object.keys(errors).length) return;
    if (!form.scopes.length) {
      formError = 'Select at least one scope.';
      return;
    }
    const saved = await onSubmit(
      buildApiKeyPolicyInput(form),
      form.allowedRoutes[0]
    );
    if (saved) dirty = false;
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
      onclick={onCancel}>Cancel</button
    >{:else}<a class="button button-secondary" href={resolve('/api-keys')}
      >Cancel</a
    >{/if}
</div>
{#if submitError || formError}<div class="inline-problem" role="alert">
    {submitError || formError}
  </div>{/if}
{#if !canManage}
  <ReadOnlyNote>
    {editing
      ? 'This API key policy can be viewed but not changed.'
      : 'Your role can view API key policies but not create or change them.'}
  </ReadOnlyNote>
{/if}

<form
  class="card key-form"
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
          bind:value={form.requestsPerMinute}
          disabled={!canManage}
          aria-invalid={errors.requestsPerMinute ? 'true' : undefined}
        />{#if errors.requestsPerMinute}<small class="field-error"
            >{errors.requestsPerMinute}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="tpm">Tokens per minute</label><input
          id="tpm"
          type="number"
          min="1"
          inputmode="numeric"
          bind:value={form.tokensPerMinute}
          disabled={!canManage}
          aria-invalid={errors.tokensPerMinute ? 'true' : undefined}
        />{#if errors.tokensPerMinute}<small class="field-error"
            >{errors.tokensPerMinute}</small
          >{/if}
      </div>
      <div class="form-field">
        <label for="concurrency">Concurrent requests</label><input
          id="concurrency"
          type="number"
          min="1"
          inputmode="numeric"
          bind:value={form.maxConcurrency}
          disabled={!canManage}
          aria-invalid={errors.maxConcurrency ? 'true' : undefined}
        />{#if errors.maxConcurrency}<small class="field-error"
            >{errors.maxConcurrency}</small
          >{/if}
      </div>
    </div>
  </section>
  <section aria-labelledby="budget-heading">
    <p class="eyebrow">Spend controls</p>
    <h2 id="budget-heading">Cost budgets</h2>
    <p class="section-help">
      Amounts use the installation pricing currency. Daily and monthly windows
      reset at UTC boundaries and always fail closed if Valkey is unavailable.
      Leave blank for no cost budget.
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
            >{formatCost(editing.budget.daily.accrued)} / {editing.budget.daily
              .limit === null
              ? 'No limit'
              : formatCost(editing.budget.daily.limit)}</strong
          >
          <small
            >Window ends {formatDate(
              editing.budget.daily.window_ends_at
            )}</small
          >
        </div>
        <div>
          <span>Monthly accrued / limit</span>
          <strong
            >{formatCost(editing.budget.monthly.accrued)} / {editing.budget
              .monthly.limit === null
              ? 'No limit'
              : formatCost(editing.budget.monthly.limit)}</strong
          >
          <small
            >Window ends {formatDate(
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
  {#if canManage || !editing}<div class="form-actions">
      <button
        class="button button-primary"
        type="submit"
        disabled={!canManage || Boolean(busy)}
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
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .key-form {
    display: grid;
    max-width: 66rem;
    gap: 2rem;
    margin-top: 1.5rem;
    padding: clamp(1.2rem, 3vw, 2rem);
  }
  .key-form section + section {
    padding-top: 1.5rem;
    border-top: 1px solid var(--border);
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
    font-weight: 700;
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
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
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
    gap: 0.2rem;
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .budget-detail span,
  .budget-detail small {
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .field-error {
    color: var(--danger) !important;
    font-weight: 700;
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 48rem) {
    .limits,
    .budget-inputs,
    .budget-detail {
      grid-template-columns: 1fr;
    }
  }
</style>
