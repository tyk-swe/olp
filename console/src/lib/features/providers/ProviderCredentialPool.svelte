<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { apiClient } from '$lib/api/client';
  import { result, errorMessage } from '$lib/api/http';
  import type { components } from '$lib/api/schema';
  import type { Provider } from './api';
  import { parseManualModelNames } from './providerEditor';
  type Slot = components['schemas']['CredentialSlot'];
  let {
    provider,
    canManage,
    onChanged
  }: {
    provider: Provider;
    canManage: boolean;
    onChanged: () => void | Promise<void>;
  } = $props();
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  let editing = $state<Slot | null>(null);
  let editingEtag = $state('');
  let secret = $state('');
  let models = $state('');
  let routes = $state('');
  let keys = $state('');
  const pool = createQuery(() => ({
    queryKey: ['provider-slots', provider.id, provider.etag],
    queryFn: async () => {
      const response = await apiClient.GET(
        '/api/v3/providers/{provider_id}/credential-slots',
        { params: { path: { provider_id: provider.id } } }
      );
      return result(response.data, response.error, response.response);
    }
  }));
  function edit(slot?: Slot) {
    editingEtag = pool.data?.etag ?? '';
    editing = slot
      ? structuredClone(slot)
      : {
          id: crypto.randomUUID(),
          name: '',
          enabled: true,
          priority: 0,
          weight: 1,
          credential_version_id: null,
          allowed_models: [],
          allowed_routes: [],
          allowed_api_keys: [],
          requests_per_minute: null,
          tokens_per_minute: null,
          max_concurrency: null
        };
    secret = '';
    models = slot?.allowed_models?.join(', ') ?? '';
    routes = slot?.allowed_routes?.join(', ') ?? '';
    keys = slot?.allowed_api_keys?.join(', ') ?? '';
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || busy || !editing || !editingEtag) return;
    busy = 'save';
    error = '';
    notice = '';
    try {
      const response = await apiClient.PUT(
        '/api/v3/providers/{provider_id}/credential-slots/{slot_id}',
        {
          params: { path: { provider_id: provider.id, slot_id: editing.id! } },
          headers: {
            'If-Match': editingEtag,
            'Idempotency-Key': crypto.randomUUID()
          },
          body: {
            slot: {
              ...editing,
              allowed_models: parseManualModelNames(models),
              allowed_routes: parseManualModelNames(routes),
              allowed_api_keys: parseManualModelNames(keys)
            },
            credential: secret || null
          }
        }
      );
      result(response.data, response.error, response.response);
      secret = '';
      editing = null;
      notice =
        'Credential staged. Validate its model access, then test and activate the completed provider draft.';
      await onChanged();
      await pool.refetch();
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = '';
    }
  }
  async function validate(slot: Slot) {
    if (!pool.data) return;
    busy = slot.id!;
    error = '';
    notice = '';
    try {
      const response = await apiClient.POST(
        '/api/v3/providers/{provider_id}/credential-slots/{slot_id}/validate',
        {
          params: { path: { provider_id: provider.id, slot_id: slot.id! } },
          headers: { 'If-Match': pool.data.etag }
        }
      );
      result(response.data, response.error, response.response);
      notice = `${slot.name}: model access validated.`;
      await pool.refetch();
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = '';
    }
  }
</script>

<section class="card pool" aria-labelledby="credential-pool-heading">
  <h2 id="credential-pool-heading">Credential pool</h2>
  <p>
    Use multiple accounts or keys on this connection. Lower priorities are tried
    first; weights distribute requests within a priority.
  </p>
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if notice}<p class="success-banner" role="status">{notice}</p>{/if}
  {#if pool.isError}<p role="alert">
      Credentials could not be loaded. <button
        type="button"
        onclick={() => pool.refetch()}>Retry</button
      >
    </p>{/if}
  {#if pool.isPending}<p role="status">Loading credentials…</p>{/if}
  {#if pool.data?.connection_usage}<p>
      Connection usage this minute: {pool.data.connection_usage
        .requests_this_minute} requests · {pool.data.connection_usage
        .tokens_this_minute} tokens · {pool.data.connection_usage
        .concurrent_requests} concurrent requests.
    </p>{/if}
  <ul>
    {#each pool.data?.items ?? [] as slot (slot.id)}
      <li>
        <div>
          <strong>{slot.name}</strong><span
            >{slot.enabled ? 'Enabled' : 'Disabled'} · Priority {slot.priority} ·
            Weight {slot.weight}</span
          >{#if pool.data?.health?.[slot.id]}{@const health =
              pool.data.health[slot.id]}<small
              >{health.active_credential_version_id ===
                slot.credential_version_id && slot.credential_version_id
                ? 'Active'
                : health.active_credential_version_id
                  ? 'Change staged'
                  : 'Draft'} · {health.cooling_down
                ? 'Cooling down'
                : health.cooling_down === false
                  ? 'Available'
                  : 'Live health unknown'} · {health.validated_at
                ? 'Access validated'
                : 'Access validation required'}</small
            >{#if health.usage}<small
                >{health.usage.requests_this_minute} / {slot.requests_per_minute ??
                  'unlimited'} requests this minute · {health.usage
                  .tokens_this_minute} / {slot.tokens_per_minute ?? 'unlimited'} tokens
                · {health.usage.concurrent_requests} / {slot.max_concurrency ??
                  'unlimited'} concurrent</small
              >{/if}{/if}<small
            >{slot.allowed_models?.length || 'All'} models · {slot
              .allowed_routes?.length || 'All'} routes</small
          >
        </div>
        {#if canManage}<button
            class="button button-secondary"
            type="button"
            disabled={Boolean(busy)}
            onclick={() => edit(slot)}>Edit / rotate</button
          ><button
            class="button button-secondary"
            type="button"
            disabled={Boolean(busy) || !slot.credential_version_id}
            onclick={() => validate(slot)}
            >{busy === slot.id ? 'Validating…' : 'Validate access'}</button
          >{/if}
      </li>
    {/each}
  </ul>
  {#if canManage && !editing}<button
      class="button button-secondary"
      type="button"
      disabled={!pool.data || pool.isFetching || Boolean(busy)}
      onclick={() => edit()}>Add credential</button
    >{/if}
  {#if editing}
    <form onsubmit={save}>
      <fieldset disabled={!canManage || Boolean(busy)}>
        <legend class="sr-only">Credential settings</legend>
        <div class="form-grid">
          <label>Name<input required bind:value={editing.name} /></label>
          <label
            >Priority<input
              type="number"
              min="0"
              max="65535"
              bind:value={editing.priority}
            /></label
          >
          <label
            >Weight<input
              type="number"
              min="1"
              bind:value={editing.weight}
            /></label
          >
          <label
            >Credential<input
              type="password"
              autocomplete="new-password"
              bind:value={secret}
              placeholder="Leave blank to retain the stored secret"
            /></label
          >
          <label
            >Allowed models<input
              bind:value={models}
              placeholder="All models, or comma-separated upstream IDs"
            /></label
          >
          <label
            >Allowed routes<input
              bind:value={routes}
              placeholder="All routes, or comma-separated route names"
            /></label
          >
          <label
            >Allowed gateway keys<input
              bind:value={keys}
              placeholder="All keys, or comma-separated key IDs"
            /></label
          >
          <label
            >Requests / minute<input
              type="number"
              min="1"
              bind:value={editing.requests_per_minute}
            /></label
          >
          <label
            >Tokens / minute<input
              type="number"
              min="1"
              bind:value={editing.tokens_per_minute}
            /></label
          >
          <label
            >Concurrent requests<input
              type="number"
              min="1"
              bind:value={editing.max_concurrency}
            /></label
          >
          <label
            ><input type="checkbox" bind:checked={editing.enabled} /> Enabled</label
          >
        </div>
        <button class="button button-primary" disabled={Boolean(busy)}
          >Save credential</button
        >
        <button
          class="button button-secondary"
          type="button"
          onclick={() => {
            editing = null;
            secret = '';
          }}>Cancel</button
        >
      </fieldset>
    </form>
  {/if}
</section>

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
  }
  .pool {
    padding: 1.5rem;
    margin-top: 1.5rem;
  }
  p,
  span,
  small {
    color: var(--foreground-muted);
  }
  ul {
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.65rem;
    padding: 0.85rem 0;
    border-top: 1px solid var(--border);
  }
  li div {
    display: grid;
    gap: 0.2rem;
    flex: 1;
    min-width: 12rem;
  }
  label {
    display: grid;
    gap: 0.35rem;
  }
  fieldset > button {
    margin-top: 1rem;
  }
</style>
