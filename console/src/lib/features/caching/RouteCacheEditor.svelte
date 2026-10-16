<script lang="ts">
  import type { RouteDraftEditorState } from '$lib/features/routes/routeDraftEditor.svelte';
  import type { components } from '$lib/api/schema';
  import { errorMessage } from '$lib/api/http';
  import { listRoutes } from '$lib/features/routes/api';
  import { cacheStatistics, purgeCache } from './api';

  let { editor }: { editor: RouteDraftEditorState } = $props();
  const locked = $derived(!editor.canManage || Boolean(editor.busy));
  let status = $state<components['schemas']['CacheStatistics'] | null>(null);
  let notice = $state('');
  let problem = $state('');
  let busy = $state(false);

  function setMode(mode: string) {
    editor.behavior.cache =
      mode === 'off'
        ? null
        : {
            ...editor.behavior.cache,
            mode: mode as 'exact' | 'semantic',
            scope: editor.behavior.cache?.scope ?? 'key',
            ttl_seconds: editor.behavior.cache?.ttl_seconds ?? 3600,
            max_entry_bytes: editor.behavior.cache?.max_entry_bytes ?? 1048576,
            operations:
              mode === 'semantic'
                ? ['generation']
                : ['generation', 'embeddings', 'rerank', 'moderation'],
            streaming: 'replay',
            count_hits_against: ['requests'],
            ...(mode === 'semantic'
              ? {
                  embedding_route: '',
                  similarity_threshold: 0.95,
                  max_candidates: 5,
                  allow_unsealed_vectors: false,
                  allow_multiturn: false
                }
              : {
                  embedding_route: undefined,
                  allow_unsealed_vectors: undefined,
                  allow_multiturn: undefined
                })
          };
    editor.touch();
  }
  function toggleCount(kind: 'tokens' | 'budgets', enabled: boolean) {
    const policy = editor.behavior.cache;
    if (!policy) return;
    const counts = policy.count_hits_against ?? ['requests'];
    policy.count_hits_against = enabled
      ? [...new Set([...counts, kind])]
      : counts.filter((item) => item !== kind);
    editor.touch();
  }
  async function refresh() {
    busy = true;
    problem = '';
    notice = '';
    try {
      status = await cacheStatistics(editor.slug);
    } catch (error) {
      problem = errorMessage(error);
    } finally {
      busy = false;
    }
  }
  async function purge() {
    busy = true;
    problem = '';
    notice = '';
    try {
      const routes = await listRoutes();
      const route = routes.find(
        (item) =>
          item.slug === editor.slug &&
          (item.project_id ?? '') === editor.projectId
      );
      if (!route)
        throw new Error('Publish this route before purging its cache.');
      await purgeCache({ route_id: route.id });
      notice = 'Cached responses for this route were invalidated.';
      status = await cacheStatistics(editor.slug);
    } catch (error) {
      problem = errorMessage(error);
    } finally {
      busy = false;
    }
  }
</script>

<section class="card cache-editor" aria-labelledby="cache-heading">
  <p class="eyebrow">Response caching</p>
  <h2 id="cache-heading">Reuse guarded responses</h2>
  <p class="muted">
    Cache entries are encrypted and expire automatically. Requests still pass
    authorization, admission and input guardrails.
  </p>
  <fieldset disabled={locked}>
    <legend>Cache policy</legend>
    <div class="form-field">
      <label for="cache-mode">Response cache</label>
      <select
        id="cache-mode"
        value={editor.behavior.cache?.mode ?? 'off'}
        onchange={(event) => setMode(event.currentTarget.value)}
      >
        <option value="off">Disabled</option><option value="exact"
          >Exact match</option
        ><option value="semantic">Semantic match</option>
      </select>
    </div>
    {#if editor.behavior.cache}
      <div class="form-grid">
        <div class="form-field">
          <label for="cache-scope">Sharing scope</label><select
            id="cache-scope"
            bind:value={editor.behavior.cache.scope}
            onchange={editor.touch}
            ><option value="key">This API key</option><option value="project"
              >Keys in this project</option
            ><option value="route">Keys using this route</option></select
          >
        </div>
        <div class="form-field">
          <label for="cache-ttl">Lifetime (seconds)</label><input
            id="cache-ttl"
            type="number"
            min="1"
            max="604800"
            bind:value={editor.behavior.cache.ttl_seconds}
            oninput={editor.touch}
          />
        </div>
        <div class="form-field">
          <label for="cache-bytes">Maximum entry size (bytes)</label><input
            id="cache-bytes"
            type="number"
            min="1"
            max="16777216"
            bind:value={editor.behavior.cache.max_entry_bytes}
            oninput={editor.touch}
          />
        </div>
        <div class="form-field">
          <label for="cache-streaming">Streaming responses</label><select
            id="cache-streaming"
            bind:value={editor.behavior.cache.streaming}
            onchange={editor.touch}
            ><option value="replay">Replay original events</option><option
              value="bypass">Bypass cache</option
            ></select
          >
        </div>
      </div>
      {#if editor.behavior.cache.scope !== 'key'}<p class="muted">
          This scope explicitly permits responses to be reused by other
          authorized API keys in the same project.
        </p>{/if}
      <p>Hits count against request limits.</p>
      <label
        ><input
          type="checkbox"
          checked={editor.behavior.cache.count_hits_against?.includes('tokens')}
          onchange={(event) =>
            toggleCount('tokens', event.currentTarget.checked)}
        /> Also count cached tokens against token limits</label
      >
      <label
        ><input
          type="checkbox"
          checked={editor.behavior.cache.count_hits_against?.includes(
            'budgets'
          )}
          onchange={(event) =>
            toggleCount('budgets', event.currentTarget.checked)}
        /> Also debit budgets at the current equivalent cost</label
      >
      {#if editor.behavior.cache.mode === 'semantic'}
        <p class="muted">
          Semantic matching may reuse a response to a similar question. A
          transformed route is required. Embedding calls are billed normally.
        </p>
        <div class="form-grid">
          <div class="form-field">
            <label for="cache-embedding">Embedding route</label><input
              id="cache-embedding"
              bind:value={editor.behavior.cache.embedding_route}
              oninput={editor.touch}
            />
          </div>
          <div class="form-field">
            <label for="cache-similarity">Minimum similarity</label><input
              id="cache-similarity"
              type="number"
              min="0.01"
              max="1"
              step="0.01"
              bind:value={editor.behavior.cache.similarity_threshold}
              oninput={editor.touch}
            />
          </div>
          <div class="form-field">
            <label for="cache-candidates">Maximum candidates</label><input
              id="cache-candidates"
              type="number"
              min="1"
              max="32"
              bind:value={editor.behavior.cache.max_candidates}
              oninput={editor.touch}
            />
          </div>
        </div>
        <label
          ><input
            type="checkbox"
            bind:checked={editor.behavior.cache.allow_unsealed_vectors}
            onchange={editor.touch}
          /> I acknowledge that content-derived vectors are stored unencrypted in
          the search index.</label
        >
        <label
          ><input
            type="checkbox"
            bind:checked={editor.behavior.cache.allow_multiturn}
            onchange={editor.touch}
          /> Allow matching the final question within an otherwise identical conversation</label
        >
      {/if}
    {/if}
  </fieldset>
  <fieldset disabled={locked}>
    <legend>Provider prompt caching</legend>
    <label
      ><input
        type="checkbox"
        checked={Boolean(editor.behavior.prompt_cache)}
        onchange={(event) => {
          editor.behavior.prompt_cache = event.currentTarget.checked
            ? {
                ttl: '5m',
                positions: ['system', 'tools', 'last_user'],
                key_source: 'key'
              }
            : null;
          editor.touch();
        }}
      /> Insert omitted provider cache controls</label
    >
    {#if editor.behavior.prompt_cache}
      <p class="muted">
        Available on transformed routes. Existing caller controls are preserved.
      </p>
      <div class="form-grid">
        <div class="form-field">
          <label for="prompt-cache-ttl">Anthropic cache lifetime</label><select
            id="prompt-cache-ttl"
            bind:value={editor.behavior.prompt_cache.ttl}
            onchange={editor.touch}
            ><option value="5m">5 minutes</option><option value="1h"
              >1 hour</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for="prompt-cache-source">OpenAI cache identity</label><select
            id="prompt-cache-source"
            bind:value={editor.behavior.prompt_cache.key_source}
            onchange={editor.touch}
            ><option value="key">API key</option><option value="session"
              >Session label within the API key</option
            ></select
          >
        </div>
        {#if editor.behavior.prompt_cache.key_source === 'session'}<div
            class="form-field"
          >
            <label for="prompt-cache-session">Session attribution label</label
            ><input
              id="prompt-cache-session"
              bind:value={editor.behavior.prompt_cache.session_label}
              oninput={editor.touch}
            />
          </div>{/if}
      </div>
    {/if}
  </fieldset>
  <div class="cache-actions">
    <button
      type="button"
      class="button button-secondary"
      disabled={busy}
      onclick={refresh}>Refresh cache statistics</button
    ><button
      type="button"
      class="button button-secondary"
      disabled={locked || busy || editor.isNew}
      onclick={purge}>Purge cached responses</button
    >
  </div>
  {#if status}<p role="status">
      Last 30 days: {status.hits} hits · {status.misses} misses · {status.bypasses}
      bypasses. {status.configured
        ? 'Cache connection configured.'
        : 'Cache connection is not configured.'}
    </p>
    {#each status.avoided_costs as cost (cost.currency)}<p>
        Avoided provider cost: {cost.amount}
        {cost.currency}
      </p>{/each}{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
</section>

<style>
  .cache-editor {
    display: grid;
    gap: 1rem;
  }
  fieldset {
    display: grid;
    gap: 0.75rem;
  }
  .cache-actions {
    display: flex;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
</style>
