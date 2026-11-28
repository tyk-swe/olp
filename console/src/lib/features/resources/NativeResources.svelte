<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import QueryView from '#lib/components/QueryView.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { listRoutes, routeKeys } from '#lib/features/routes/index.js';
  import { errorMessage } from '#lib/management/http.js';
  import NativeResourceFields from './NativeResourceFields.svelte';
  import {
    ResourceRequestError,
    extractResourceJsonId,
    isGatewayResourceId
  } from './lifecycle';
  import {
    listNativeEndpoints,
    nativeResourceFingerprint,
    runNativeResourceRequest,
    type NativeEndpoint,
    type NativeResourceRequest,
    type NativeResourceResult
  } from './nativeLifecycle';
  import { resourceKeys } from './resourceKeys';

  const contracts = createQuery(() => ({
    queryKey: resourceKeys.contracts(),
    queryFn: ({ signal }) => listNativeEndpoints(signal)
  }));
  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const action = useAction();
  const intent = useMutationIntent();
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
  let downloadUrl = $state('');
  let downloadName = $state('');
  let downloadBytes = $state(0);
  let controller: AbortController | undefined;

  const apiKeyMissing = $derived(!apiKey.trim());

  function clearDownload() {
    if (downloadUrl) URL.revokeObjectURL(downloadUrl);
    downloadUrl = downloadName = '';
    downloadBytes = 0;
  }

  usePrincipalLifetime(() => {
    controller?.abort();
    controller = undefined;
    clearDownload();
    apiKey = route = id = result = '';
    parents = ['', ''];
    body = query = '{}';
    files = [];
    filePickerKey += 1;
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

  const key = (item: NativeEndpoint) => `${item.method} ${item.pattern}`;

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
    body = query = '{}';
    result = '';
    action.error = '';
    clearDownload();
  }

  function captureId(value: string | null | undefined, kind: string) {
    if (value && isGatewayResourceId(kind, value)) id = value;
  }

  async function run(event: SubmitEvent) {
    event.preventDefault();
    if (action.busy || !endpoint) return;
    const selected = endpoint;
    const owner = new AbortController();
    controller = owner;
    const request: NativeResourceRequest = {
      endpoint: selected,
      apiKey,
      route,
      id,
      parents: [...parents],
      body,
      query,
      multipart,
      files: [...files],
      signal: owner.signal
    };
    result = '';
    clearDownload();
    const accept = (response: NativeResourceResult) => {
      captureId(response.resourceId, selected.kind);
      if (response.download) {
        downloadUrl = URL.createObjectURL(response.download);
        downloadName = `${request.id.trim()}.bin`;
        downloadBytes = response.download.size;
        notify('File content is ready to download.');
        return;
      }
      result = response.text;
      notify(
        response.text
          ? 'Resource operation completed.'
          : 'Resource operation completed without a response body.'
      );
      const bodyId = extractResourceJsonId(response.text, selected.kind);
      if (bodyId) id = bodyId;
    };
    const feedback = {
      key: 'run',
      failure: (cause: unknown) => {
        if (owner.signal.aborted) return '';
        if (cause instanceof ResourceRequestError) {
          if (cause.definitiveMutationRejection) intent.reset();
          captureId(cause.resourceId, selected.kind);
        }
        return errorMessage(
          cause,
          'The resource operation could not be completed.'
        );
      }
    };
    await action.run(
      async () => {
        if (selected.method === 'GET') return runNativeResourceRequest(request);
        const fingerprint = await nativeResourceFingerprint(request);
        owner.signal.throwIfAborted();
        return intent.submit(
          [fingerprint, request.apiKey.trim()],
          (_input, submissionKey) =>
            runNativeResourceRequest({ ...request, submissionKey })
        );
      },
      accept,
      feedback
    );
    if (controller === owner) controller = undefined;
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
  <QueryView
    query={contracts}
    loading="Loading supported resource actions…"
    failure={errorMessage(
      contracts.error,
      'Resource contracts could not be loaded.'
    )}
    emptyWhen={!contracts.data?.length}
  >
    {#snippet empty()}
      <p>The server has not published selectable resource lifecycle actions.</p>
    {/snippet}
    <form onsubmit={run}>
      <fieldset disabled={action.busy}>
        <div class="resource-fields">
          <label>
            Resource family<select
              bind:value={operation}
              onchange={selectEndpoint}
            >
              {#each operations as value (value)}<option {value}>
                  {value.replaceAll('_', ' ')}
                </option>{/each}
            </select>
          </label>
          <label>
            Action<select bind:value={endpointKey} onchange={selectEndpoint}>
              {#each available as item (key(item))}<option value={key(item)}>
                  {item.action} · {item.kind.replaceAll('_', ' ')} ({item.surface})
                </option>{/each}
            </select>
          </label>
          <label>
            Inference API key<input
              type="password"
              autocomplete="off"
              bind:value={apiKey}
              required
            />
          </label>
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
      {#if action.busy}<div class="resource-actions">
          <p role="status">Waiting for the resource operation…</p>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => controller?.abort()}
          >
            Stop waiting
          </button>
        </div>{/if}
    </form>
  </QueryView>
  {#if action.error}<p class="inline-problem" role="alert">
      {action.error}
    </p>{/if}
  {#if downloadUrl}<a
      class="button button-secondary"
      href={downloadUrl}
      download={downloadName}
    >
      Download file ({downloadBytes.toLocaleString()} bytes)
    </a>{/if}
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
    max-height: 28rem;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
