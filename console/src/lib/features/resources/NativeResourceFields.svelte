<script lang="ts">
  import type { ActiveRoute } from '#lib/features/routes/index.js';
  import type { NativeEndpoint } from './nativeLifecycle';

  let {
    endpoint,
    eligibleRoutes,
    routesPending,
    routesError,
    onReloadRoutes,
    requiresRoute,
    writesBody,
    supportsMultipart,
    requiresMultipart,
    multipart,
    apiKeyMissing,
    filePickerKey,
    route = $bindable(),
    parents = $bindable(),
    id = $bindable(),
    query = $bindable(),
    body = $bindable(),
    upload = $bindable(),
    files = $bindable()
  }: {
    endpoint: NativeEndpoint;
    eligibleRoutes: ActiveRoute[];
    routesPending: boolean;
    routesError: string | null;
    onReloadRoutes: () => void;
    requiresRoute: boolean;
    writesBody: boolean;
    supportsMultipart: boolean;
    requiresMultipart: boolean;
    multipart: boolean;
    apiKeyMissing: boolean;
    filePickerKey: number;
    route: string;
    parents: string[];
    id: string;
    query: string;
    body: string;
    upload: boolean;
    files: File[];
  } = $props();
</script>

<code class="endpoint">{endpoint.method} {endpoint.pattern}</code>
{#if requiresRoute || endpoint.action === 'list'}
  <label
    >Published route{requiresRoute ? '' : ' (optional)'}<select
      bind:value={route}
      required={requiresRoute}
      disabled={routesPending}
    >
      <option value=""
        >{requiresRoute
          ? 'Choose a route'
          : 'All routes allowed by the key'}</option
      >
      {#each eligibleRoutes as item (item.slug)}<option value={item.slug}
          >{item.slug}</option
        >{/each}
    </select></label
  >
  {#if routesError}<p class="inline-problem" role="alert">
      {routesError}
    </p>
    <button class="text-button" type="button" onclick={onReloadRoutes}
      >Reload routes</button
    >{/if}
{/if}
<div class="resource-fields">
  {#each endpoint.ancestors as kind, index (index)}<label
      >{kind.replaceAll('_', ' ')} parent ID<input
        bind:value={parents[index]}
        placeholder={`${kind}_…`}
        required
      /></label
    >{/each}
  {#if endpoint.pattern.includes('{id}')}<label
      >{endpoint.kind.replaceAll('_', ' ')} ID<input
        bind:value={id}
        placeholder={`${endpoint.kind}_…`}
        required
      /></label
    >{/if}
</div>
<label
  >Query parameters (JSON string values)<textarea
    bind:value={query}
    rows="3"
    spellcheck="false"
    placeholder={'{"limit":"20","after":"gateway cursor"}'}></textarea></label
>
<small
  >Use native pagination controls and gateway cursors returned by the selected
  endpoint. Cache TTL updates can include updateMask.</small
>
{#if writesBody}
  {#if supportsMultipart && !requiresMultipart}<label class="check"
      ><input type="checkbox" bind:checked={upload} />Upload a file instead of
      referencing an existing file</label
    >{/if}
  <label
    >{multipart
      ? 'Multipart text fields (JSON string values)'
      : 'Request body (native JSON)'}<textarea
      bind:value={body}
      rows="7"
      spellcheck="false"
      placeholder={endpoint.kind === 'container_file' && !multipart
        ? '{"file_id":"gateway file ID"}'
        : '{}'}></textarea></label
  >
  <small
    >Use gateway IDs for resource references. Match the native request fields of
    the selected provider.</small
  >
  {#if multipart}
    {#key filePickerKey}<label
        >{requiresMultipart ? 'Skill files' : 'Container file'}<input
          type="file"
          multiple={requiresMultipart}
          onchange={(event) => {
            files = Array.from(event.currentTarget.files ?? []);
          }}
          required
        /></label
      >{/key}
    <small
      >Uploads are limited to 4 MiB total. Container uploads require an empty
      JSON object above.</small
    >
  {/if}
{/if}
{#if endpoint.metered && endpoint.action === 'create'}<p class="muted">
    This action can start paid provider work or retained storage. Applicable
    prices and budget restrictions are checked before dispatch.
  </p>{/if}
<button
  class="button button-primary"
  type="submit"
  disabled={apiKeyMissing ||
    (requiresRoute && !route) ||
    (multipart && !files.length)}
  >{endpoint.action === 'content'
    ? 'Fetch file content'
    : `${endpoint.action[0]?.toUpperCase()}${endpoint.action.slice(1)} resource`}</button
>

<style>
  label {
    display: grid;
    gap: 0.4rem;
  }
  .check {
    display: flex;
    flex-wrap: wrap;
    gap: 0.6rem;
    align-items: center;
  }
  textarea {
    font-family: var(--font-mono, monospace);
  }
  .endpoint {
    overflow-wrap: anywhere;
  }
  small,
  .muted {
    color: var(--foreground-muted);
  }
</style>
