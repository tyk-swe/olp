<script lang="ts">
  import { untrack } from 'svelte';
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import { parseJsonObject } from '$lib/forms/json';
  import RoutingPreferencesForm from './RoutingPreferencesForm.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { apiClient } from '$lib/api/client';
  import { result, errorMessage } from '$lib/api/http';
  import type { components } from '$lib/api/schema';
  type Policy = components['schemas']['RoutingPolicy'];
  let {
    scope,
    id,
    resourceEtag = '',
    canManage,
    dirty = $bindable(false),
    busy = $bindable(false),
    onSaved = () => {}
  }: {
    scope: 'installation' | 'route-draft' | 'api-key';
    id: string;
    resourceEtag?: string;
    canManage: boolean;
    dirty?: boolean;
    busy?: boolean;
    onSaved?: (etag: string, previousEtag: string) => void | Promise<void>;
  } = $props();
  let text = $state('{}');
  let error = $state('');
  let notice = $state('');
  let baseline = $state('');
  let conflict = $state(false);
  let loadedResource = $state('');
  let sections = $state({ constraints: '{}', defaults: '{}' });
  const invalidSections = $derived(
    Object.values(sections).some((value) => parseJsonObject(value) === null)
  );
  guardUnsavedChanges(() => dirty);
  const policy = createQuery(() => ({
    queryKey: ['routing-policy', scope, id, resourceEtag],
    queryFn: async () => {
      const response = await apiClient.GET(
        '/api/v3/routing-policies/{scope}/{id}',
        { params: { path: { scope, id } } }
      );
      return result(response.data, response.error, response.response);
    }
  }));
  $effect(() => {
    const data = policy.data;
    const resource = `${scope}:${id}`;
    if (!data) return;
    untrack(() => {
      const remote = JSON.stringify(data.policy, null, 2);
      if (resource !== loadedResource || !dirty) {
        text = remote;
        syncSections();
        baseline = remote;
        dirty = false;
        conflict = false;
        loadedResource = resource;
      } else {
        conflict = remote !== baseline;
      }
    });
  });
  async function reload() {
    const response = await policy.refetch();
    if (!response.data || response.error) return;
    text = JSON.stringify(response.data.policy, null, 2);
    syncSections();
    baseline = text;
    dirty = false;
    conflict = false;
    error = '';
  }
  const current = $derived((parseJsonObject(text) ?? {}) as Partial<Policy>);
  function syncSections() {
    sections = {
      constraints: JSON.stringify(current.constraints ?? {}),
      defaults: JSON.stringify(current.defaults ?? {})
    };
  }
  function section(key: 'constraints' | 'defaults', value: string) {
    sections[key] = value;
    dirty = true;
    notice = '';
    try {
      const next = JSON.parse(text);
      next[key] = JSON.parse(value);
      text = JSON.stringify(next, null, 2);
      error = '';
    } catch {
      error = 'Correct the policy JSON before changing fields.';
    }
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!policy.data || conflict || policy.isFetching || invalidSections)
      return;
    busy = true;
    error = '';
    notice = '';
    try {
      const body: Policy = JSON.parse(text);
      const previousEtag = policy.data.etag;
      const response = await apiClient.PUT(
        '/api/v3/routing-policies/{scope}/{id}',
        {
          params: { path: { scope, id } },
          headers: {
            'If-Match': previousEtag,
            'Idempotency-Key': crypto.randomUUID()
          },
          body
        }
      );
      const saved = result(response.data, response.error, response.response);
      dirty = false;
      await onSaved(saved.etag, previousEtag);
      await policy.refetch();
      notice =
        scope === 'route-draft'
          ? 'Routing policy staged. Validate and publish the route to apply it.'
          : 'Routing policy published.';
    } catch (e) {
      error = errorMessage(e, 'Enter a valid routing policy.');
    } finally {
      busy = false;
    }
  }
</script>

<section class="card policy">
  <h2>Provider routing policy</h2>
  <p>
    Restrictions apply together with installation, route, and API-key policies.
    Requests can narrow provider access.
  </p>
  {#if policy.isError}<p role="alert">
      Policy could not be loaded. <button
        type="button"
        onclick={() => policy.refetch()}>Retry</button
      >
    </p>{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if invalidSections}<p class="inline-problem" role="alert">
      Enter valid JSON objects in both preference sections before saving.
    </p>{/if}
  {#if conflict || error || invalidSections}<p>
      Your input is preserved. Reload to use the latest saved policy.
      <button type="button" disabled={busy} onclick={reload}
        >Reload policy</button
      >
    </p>{/if}
  {#if notice}<p class="success-banner" role="status">{notice}</p>{/if}
  {#if policy.data}
    <form onsubmit={save}>
      <fieldset disabled={!canManage || busy || policy.isFetching}>
        <legend>Policy</legend>
        <RoutingPreferencesForm
          bind:value={sections.constraints}
          id="policy-constraints-{scope}-{id}"
          constraintsOnly
          disabled={!canManage || busy}
          onChange={(value) => section('constraints', value)}
        />
        <RoutingPreferencesForm
          bind:value={sections.defaults}
          id="policy-defaults-{scope}-{id}"
          disabled={!canManage || busy}
          onChange={(value) => section('defaults', value)}
        />
        <details>
          <summary>Advanced policy and permitted strategies</summary>
          <label for="routing-policy-{scope}-{id}">Policy configuration</label>
          <textarea
            id="routing-policy-{scope}-{id}"
            rows="16"
            spellcheck="false"
            oninput={(event) => {
              text = event.currentTarget.value;
              dirty = true;
              notice = '';
              syncSections();
            }}
            bind:value={text}></textarea>
          <p class="help">
            Use <code>constraints.only</code> or <code>constraints.ignore</code>
            with <code>vendor:deepseek</code> or <code>provider:UUID</code>.
            Privacy controls are <code>deny_data_collection</code> and
            <code>require_zero_data_retention</code>. Set
            <code>require_parameters</code>
            to require declared parameter support. Defaults support
            <code>order</code>, <code>strategy</code>, and
            <code>allow_fallbacks</code>.
          </p>
        </details>
        {#if canManage}<button
            class="button button-primary"
            type="submit"
            disabled={conflict || invalidSections}
            >{busy ? 'Saving…' : 'Save routing policy'}</button
          >{/if}
      </fieldset>
    </form>
  {/if}
</section>

<style>
  .policy {
    padding: 1.5rem;
    margin-top: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  p {
    color: var(--foreground-muted);
  }
  fieldset {
    border: 0;
    padding: 0;
  }
  /* The textarea sits outside .form-field, so the shared control recipe is
     restated here. */
  textarea {
    display: block;
    width: 100%;
    min-height: 7rem;
    margin-top: 0.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-family: var(--font-mono);
    resize: vertical;
    transition: border-color var(--motion);
  }
  textarea:hover {
    border-color: var(--border-strong);
  }
  .help {
    font-size: 0.85rem;
    line-height: 1.5;
  }
</style>
