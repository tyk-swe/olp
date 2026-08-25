<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import type { ApiKey } from '$lib/api/management/api-keys';
  import { listRoutes } from '$lib/api/management/routes';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import { dateTimeLocalValue } from '$lib/format';
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import { validateApiKey } from './keyValidation';
  import type { ApiKeyPolicyInput } from './apiKeyPolicy';

  let {
    editing,
    busy,
    errorMessage,
    canManage,
    onSubmit,
    onCancel,
    onClearError
  }: {
    editing: ApiKey | null;
    busy: string;
    errorMessage: string;
    canManage: boolean;
    onSubmit: (
      input: ApiKeyPolicyInput,
      preferredRoute?: string
    ) => boolean | Promise<boolean>;
    onCancel: () => void;
    onClearError: () => void;
  } = $props();

  let name = $state('');
  let scopes = $state<string[]>(['inference']);
  let allowedRoutes = $state<string[]>([]);
  let requestsPerMinute = $state('');
  let tokensPerMinute = $state('');
  let maxConcurrency = $state('');
  let expiresAt = $state('');
  let errors = $state<Record<string, string>>({});
  let formError = $state('');
  let initialized = $state(false);
  let dirty = $state(false);
  const routes = createQuery(() => ({
    queryKey: ['routes'],
    queryFn: ({ signal }) => listRoutes(signal)
  }));

  $effect(() => {
    if (initialized) return;
    initialized = true;
    if (!editing) return;
    name = editing.name;
    scopes = [...editing.scopes];
    allowedRoutes = [...editing.allowed_routes];
    requestsPerMinute = editing.requests_per_minute?.toString() ?? '';
    tokensPerMinute = editing.tokens_per_minute?.toString() ?? '';
    maxConcurrency = editing.max_concurrency?.toString() ?? '';
    expiresAt = editing.expires_at
      ? dateTimeLocalValue(editing.expires_at)
      : '';
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
      name,
      requestsPerMinute: numberValue(requestsPerMinute),
      tokensPerMinute: numberValue(tokensPerMinute),
      maxConcurrency: numberValue(maxConcurrency),
      expiresAt
    });
    if (Object.keys(errors).length) return;
    if (!scopes.length) {
      formError = 'Select at least one scope.';
      return;
    }
    const saved = await onSubmit(
      {
        name: name.trim(),
        scopes,
        allowed_routes: allowedRoutes,
        requests_per_minute: numberValue(requestsPerMinute) ?? null,
        tokens_per_minute: numberValue(tokensPerMinute) ?? null,
        max_concurrency: numberValue(maxConcurrency) ?? null,
        expires_at: expiresAt ? new Date(expiresAt).toISOString() : null
      },
      allowedRoutes[0]
    );
    if (saved) dirty = false;
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Access · API Keys</p>
    <h1 class="page-title">
      {editing ? 'Edit key policy.' : 'Create a proxy key.'}
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
{#if errorMessage || formError}<div class="inline-problem" role="alert">
    {errorMessage || formError}
  </div>{/if}
{#if !canManage}
  <p class="read-only-note" role="note">
    Your role can view API key policies but not create or change them.
  </p>
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
          bind:value={name}
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
          bind:value={expiresAt}
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
            checked={scopes.includes(scope[0])}
            disabled={!canManage}
            onchange={(event) =>
              (scopes = toggle(scopes, scope[0], event.currentTarget.checked))}
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
              checked={allowedRoutes.includes(route.slug)}
              disabled={!canManage}
              onchange={(event) =>
                (allowedRoutes = toggle(
                  allowedRoutes,
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
      Configured limits fail closed if Valkey is unavailable. Leave blank for no
      limit.
    </p>
    <div class="form-grid limits">
      <div class="form-field">
        <label for="rpm">Requests per minute</label><input
          id="rpm"
          type="number"
          min="1"
          inputmode="numeric"
          bind:value={requestsPerMinute}
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
          bind:value={tokensPerMinute}
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
          bind:value={maxConcurrency}
          disabled={!canManage}
          aria-invalid={errors.maxConcurrency ? 'true' : undefined}
        />{#if errors.maxConcurrency}<small class="field-error"
            >{errors.maxConcurrency}</small
          >{/if}
      </div>
    </div>
  </section>
  <div class="form-actions">
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
  </div>
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
    .limits {
      grid-template-columns: 1fr;
    }
  }
</style>
