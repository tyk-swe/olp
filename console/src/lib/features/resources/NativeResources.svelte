<script lang="ts">
  import NativeResourceFields from './NativeResourceFields.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { listRoutes, routeKeys } from '#lib/features/routes/index.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
  import { ResourceRequestError } from './lifecycle';
  import {
    listNativeEndpoints,
    nativeResourceFingerprint,
    runNativeResourceRequest,
    type NativeEndpoint,
    type NativeResourceRequest
  } from './nativeLifecycle';

  const contracts = createQuery(() => ({
    queryKey: ['provider-resource-contracts'],
    queryFn: ({ signal }) => listNativeEndpoints(signal)
  }));
  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  let operation = $state('');
  let endpointKey = $state('');
  let apiKey = $state('');
  let route = $state('');
  let id = $state('');
  let parents = $state<string[]>(['', '']);
  let body = $state('{}');
  let query = $state('{}');
  let upload = $state(false);
  let files = $state<File[]>([]);
  let filePickerKey = $state(0);
  let result = $state('');
  let problem = $state('');
  let notice = $state('');
  const runAction = useAction();
  const busy = $derived(runAction.busy);
  const apiKeyMissing = $derived(!apiKey.trim());
  let downloadUrl = $state('');
  let downloadName = $state('');
  let downloadBytes = $state(0);
  let controller: AbortController | undefined;
  let pendingFingerprint = '';
  let pendingKey = '';
  let pendingApiKey = '';
  let epoch = 0;

  function clearDownload() {
    if (downloadUrl) URL.revokeObjectURL(downloadUrl);
    downloadUrl = '';
    downloadName = '';
    downloadBytes = 0;
  }
  const captureOwner = usePrincipalLifetime(() => {
    epoch += 1;
    controller?.abort();
    clearDownload();
    apiKey = '';
    route = '';
    id = '';
    parents = ['', ''];
    body = '{}';
    query = '{}';
    files = [];
    filePickerKey += 1;
    result = '';
    problem = '';
    notice = '';
    pendingFingerprint = pendingKey = pendingApiKey = '';
  });
  const operations = $derived([
    ...new Set((contracts.data ?? []).map((item) => item.operation))
  ]);
  const available = $derived(
    (contracts.data ?? []).filter((item) => item.operation === operation)
  );
  const endpoint = $derived(
    available.find((item) => key(item) === endpointKey)
  );
  const eligibleRoutes = $derived(
    (routes.data ?? []).filter(
      (item) =>
        (item.state === 'active' || endpoint?.action === 'list') &&
        item.latest_revision.operations.includes(operation)
    )
  );
  const writesBody = $derived(
    endpoint?.method === 'POST' || endpoint?.method === 'PATCH'
  );
  const requiresMultipart = $derived(
    endpoint?.operation === 'skill' && endpoint.action === 'create'
  );
  const supportsMultipart = $derived(
    requiresMultipart ||
      (endpoint?.kind === 'container_file' && endpoint.action === 'create')
  );
  const multipart = $derived(
    requiresMultipart || (supportsMultipart && upload)
  );
  const requiresRoute = $derived(
    endpoint?.action === 'create' && endpoint.ancestors.length === 0
  );

  function key(item: NativeEndpoint) {
    return `${item.method} ${item.pattern}`;
  }
  $effect(() => {
    if (!operations.includes(operation)) operation = operations[0] ?? '';
    if (!available.some((item) => key(item) === endpointKey))
      endpointKey = available[0] ? key(available[0]) : '';
  });
  $effect(() => {
    if (route && !eligibleRoutes.some((item) => item.slug === route))
      route = '';
  });
  function selectEndpoint() {
    files = [];
    filePickerKey += 1;
    upload = false;
    body = '{}';
    query = '{}';
    result = '';
    notice = '';
    problem = '';
    clearDownload();
  }
  function captureId(
    value: string | null | undefined,
    selected: NativeEndpoint
  ) {
    if (value && new RegExp(`^${selected.kind}_[a-f0-9]{32}$`).test(value))
      id = value;
  }
  async function run(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !endpoint) return;
    const selected = endpoint;
    const version = epoch;
    const isCurrent = captureOwner();
    const request: NativeResourceRequest = {
      endpoint: selected,
      apiKey,
      route,
      id,
      parents: [...parents],
      body,
      query,
      multipart,
      files: [...files]
    };
    controller = new AbortController();
    request.signal = controller.signal;
    problem = notice = result = '';
    clearDownload();
    await runAction.run(
      async () => {
        if (selected.method !== 'GET') {
          const fingerprint = await nativeResourceFingerprint(request);
          if (version !== epoch || request.signal?.aborted) return;
          if (
            fingerprint !== pendingFingerprint ||
            request.apiKey !== pendingApiKey
          ) {
            pendingFingerprint = fingerprint;
            pendingApiKey = request.apiKey;
            pendingKey = crypto.randomUUID();
          }
          request.submissionKey = pendingKey;
        }
        return runNativeResourceRequest(request);
      },
      (response, current) => {
        if (!response || !current() || version !== epoch) return;
        captureId(response.resourceId, selected);
        if (response.download) {
          downloadUrl = URL.createObjectURL(response.download);
          downloadName = `${request.id.trim()}.bin`;
          downloadBytes = response.download.size;
          notice = 'File content is ready to download.';
        } else {
          result = response.text;
          notice = response.text
            ? 'Resource operation completed.'
            : 'Resource operation completed without a response body.';
          if (response.text) {
            const parsed = captureResult(() => parseNativeJSON(response.text));
            if (parsed.ok && nativeObject(parsed.value)) {
              if (typeof parsed.value.id === 'string')
                captureId(parsed.value.id, selected);
              if (typeof parsed.value.name === 'string')
                captureId(parsed.value.name, selected);
            }
          }
        }
        if (selected.method !== 'GET')
          pendingFingerprint = pendingKey = pendingApiKey = '';
      },
      {
        key: 'run',
        failure: (cause) => {
          if (version !== epoch || request.signal?.aborted) return problem;
          if (cause instanceof ResourceRequestError)
            captureId(cause.resourceId, selected);
          return errorMessage(
            cause,
            'The resource operation could not be completed.'
          );
        }
      }
    );
    if (version === epoch && isCurrent()) problem = runAction.error;
  }
</script>

<section class="card native-resources" aria-labelledby="native-resources-title">
  <div>
    <h2 id="native-resources-title">Native resource lifecycle</h2>
    <p>
      Create, inspect and manage retained resources using an inference API key.
      The key, request content and results stay in this page until you leave.
    </p>
  </div>
  {#if contracts.isPending}<p role="status">
      Loading supported resource actions…
    </p>
  {:else if contracts.isError}<p class="inline-problem" role="alert">
      {errorMessage(contracts.error, 'Resource contracts could not be loaded.')}
    </p>
    <button
      class="text-button"
      type="button"
      onclick={() => contracts.refetch()}>Retry</button
    >
  {:else if !contracts.data?.length}<p>
      The server has not published selectable resource lifecycle actions.
    </p>
  {:else}
    <form onsubmit={run}>
      <fieldset disabled={busy}>
        <div class="resource-fields">
          <label
            >Resource family<select
              bind:value={operation}
              onchange={selectEndpoint}
            >
              {#each operations as value (value)}<option {value}
                  >{value.replaceAll('_', ' ')}</option
                >{/each}
            </select></label
          >
          <label
            >Action<select bind:value={endpointKey} onchange={selectEndpoint}>
              {#each available as item (key(item))}<option value={key(item)}
                  >{item.action} · {item.kind.replaceAll('_', ' ')} ({item.surface})</option
                >{/each}
            </select></label
          >
          <label
            >Inference API key<input
              type="password"
              autocomplete="off"
              bind:value={apiKey}
              required
            /></label
          >
        </div>
        {#if endpoint}
          <NativeResourceFields
            {endpoint}
            {eligibleRoutes}
            routesPending={routes.isPending}
            routesError={routes.isError
              ? errorMessage(routes.error, 'Published routes are unavailable.')
              : null}
            onReloadRoutes={() => routes.refetch()}
            {requiresRoute}
            {writesBody}
            {supportsMultipart}
            {requiresMultipart}
            {multipart}
            {apiKeyMissing}
            {filePickerKey}
            bind:route
            bind:parents
            bind:id
            bind:query
            bind:body
            bind:upload
            bind:files
          />
        {/if}
      </fieldset>
      {#if busy}<div class="resource-actions">
          <p role="status">Waiting for the resource operation…</p>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => controller?.abort()}>Stop waiting</button
          >
        </div>{/if}
    </form>
  {/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
  {#if downloadUrl}<a
      class="button button-secondary"
      href={downloadUrl}
      download={downloadName}
      >Download file ({downloadBytes.toLocaleString()} bytes)</a
    >{/if}
  {#if result}<pre aria-label="Native resource result">{result}</pre>{/if}
</section>

<style>
  .native-resources,
  form,
  fieldset {
    display: grid;
    gap: 1rem;
  }
  .native-resources {
    margin-block: 1rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
  }
  .resource-fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 1rem;
  }
  label {
    display: grid;
    gap: 0.4rem;
  }
  .resource-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.6rem;
    align-items: center;
  }
  pre {
    font-family: var(--font-mono, monospace);
  }
  pre {
    max-height: 28rem;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
