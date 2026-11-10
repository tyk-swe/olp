<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import { formatDate } from '#lib/format.js';
  import { listProviders } from '#lib/features/providers/providers.js';
  import { listCredentialSlots } from '#lib/features/providers/credentials.js';
  import {
    listCodeAccounts,
    listCodePools,
    listCodeRoutes,
    publishCodeRoute,
    type CodeClientConfiguration,
    type CodeClientSelection,
    type CodeRoute
  } from './api.js';
  import ClientConfiguration from './ClientConfiguration.svelte';
  import LoadError from './LoadError.svelte';
  import RouteEditor from './RouteEditor.svelte';
  import RouteRevisions from './RouteRevisions.svelte';
  import { codeKeys } from './codeKeys';
  import { adapterLabel, mutationError } from './presentation';
  import { managementSections } from './sections';

  const publishIntent = useMutationIntent();

  let {
    projectId,
    allowed,
    gatewayURL,
    loadClientConfiguration,
    onDraftChange
  }: {
    projectId: string;
    allowed: boolean;
    gatewayURL: string;
    onDraftChange: (dirty: boolean) => void;
    loadClientConfiguration?: (
      route: CodeRoute,
      selection: CodeClientSelection,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();

  const section = managementSections.routes;
  const client = useQueryClient();
  let editing = $state<{ current?: CodeRoute } | null>(null);
  let paging = $state(emptyCursorHistory());
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let detail = $state('');
  const filters = $derived({ project_id: projectId, cursor: paging.cursor });
  const routes = createQuery(() => ({
    queryKey: codeKeys.collection('routes', filters),
    queryFn: ({ signal }) => listCodeRoutes(filters, signal)
  }));
  const choices = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, 'routes'),
    queryFn: async ({ signal }) => ({
      providers: await Promise.all(
        (await listProviders(signal))
          .filter(
            (provider) =>
              provider.project_id === projectId &&
              provider.state === 'active' &&
              provider.kind !== 'plugin'
          )
          .map(async (provider) => ({
            id: provider.id,
            name: provider.name,
            slots: (
              await listCredentialSlots(provider.id, signal)
            ).items.filter((slot) => slot.enabled && slot.credential_version_id)
          }))
      ),
      accounts: await collectCursorPages((cursor) =>
        listCodeAccounts({ project_id: projectId, cursor }, signal)
      ),
      pools: await collectCursorPages((cursor) =>
        listCodePools({ project_id: projectId, cursor }, signal)
      )
    }),
    enabled: !!editing
  }));

  async function refresh() {
    error = '';
    notice = '';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Could not refresh. ${errorMessage(e)}`;
    }
  }

  async function onSaved() {
    editing = null;
    onDraftChange(false);
    notice = 'Draft saved. Publish to change serving configuration.';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Changes saved, but refreshing failed. Use Refresh to reload. ${errorMessage(e)}`;
    }
  }

  async function publish(route: CodeRoute) {
    if (
      busy ||
      !allowed ||
      !(await confirmAction({
        title: `Publish ${route.slug}?`,
        body: `Provider connections are captured without a test request.${route.codex_cli_identity ? ' Codex CLI identity compatibility fills Originator=codex_cli_rs only when every supplied Originator value is absent or blank on Codex subscription HTTP and WebSocket requests. Explicit identity headers, User-Agent, prompts, tools, reasoning and state are kept. This may break the provider’s terms and risk account suspension.' : ' Codex CLI identity compatibility is off. Identity headers and prompts are kept.'}`,
        confirmLabel: 'Publish'
      }))
    )
      return;
    busy = true;
    error = '';
    notice = '';
    try {
      const submission = publishIntent.prepare({
        id: route.id,
        etag: route.etag
      });
      await publishCodeRoute(submission.input, submission.requestId);
      publishIntent.reset();
      notice =
        'Route published without synthetic inference. Serving health and qualification remain separate.';
      try {
        await client.invalidateQueries(
          { queryKey: codeKeys.root },
          { throwOnError: true }
        );
      } catch (e) {
        error = `Route published, but refreshing failed. ${errorMessage(e)}`;
      }
    } catch (e) {
      error = mutationError(e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="section">
  <div class="actions">
    <h2>{section.title}</h2>
    {#if allowed}
      <button
        class="button button-primary"
        type="button"
        disabled={!!editing || busy}
        onclick={() => (editing = {})}>Create {section.noun}</button
      >
    {/if}
    <button
      class="button button-secondary"
      type="button"
      onclick={refresh}
      disabled={busy || routes.isFetching}>Refresh</button
    >
  </div>
  {#if !allowed}
    <ReadOnlyNote>
      Read only. Changes require configure permission and project manager or
      global authority.
    </ReadOnlyNote>
  {/if}
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if editing}
    {#if choices.isError}
      <LoadError error={choices.error} onRetry={() => choices.refetch()}>
        <button
          class="button button-secondary"
          type="button"
          onclick={() => (editing = null)}>Cancel</button
        >
      </LoadError>
    {:else if choices.data}
      {#key editing}
        <RouteEditor
          current={editing.current}
          {projectId}
          {...choices.data}
          {allowed}
          {onDraftChange}
          {onSaved}
          onCancel={() => (editing = null)}
        />
      {/key}
    {:else}
      <p role="status">Loading editor choices…</p>
    {/if}
  {/if}
  {#if routes.isPending}
    <p role="status">Loading routes…</p>
  {:else if routes.isError}
    <LoadError error={routes.error} onRetry={() => routes.refetch()} />
  {:else if !routes.data?.items.length}
    <p class="empty">No {section.empty} in this project.</p>
  {:else}
    <p>
      Route base paths select policy; the client sends native models unchanged.
      A pool may mix Codex, OpenCode Go and GLM Coding Plan accounts: each
      request goes to an account that lists its model and whose subscription
      serves the client's protocol. Connection edits require route republish.
      Pool/account/key permissions remain live.
    </p>
    {#each routes.data.items as route (route.id)}
      <article class="card">
        <div class="actions">
          <h3>{route.slug}</h3>
          {#if allowed}
            <button
              class="button button-secondary"
              type="button"
              disabled={!!editing || busy}
              onclick={() => (editing = { current: route })}>Edit draft</button
            >
            <button
              class="button button-primary"
              type="button"
              disabled={!!editing || busy}
              onclick={() => publish(route)}>Publish route</button
            >
          {/if}
          <button
            class="button button-secondary"
            type="button"
            aria-expanded={detail === route.id}
            onclick={() => (detail = detail === route.id ? '' : route.id)}
            >Revisions and client setup</button
          >
        </div>
        <dl>
          <dt>Base path</dt>
          <dd><code>/code/{route.slug}</code></dd>
          <dt>Published subscriptions</dt>
          <dd class="badges">
            {#each route.adapters as adapter (adapter)}
              <span class="badge">{adapterLabel(adapter)}</span>
            {:else}
              {route.published_at
                ? 'No subscription adapter is published'
                : 'Set at publication'}
            {/each}
          </dd>
          <dt>Draft allowance ordering</dt>
          <dd>
            {route.allowance_ordering
              ? 'Configured, requires publication and a qualified model-to-limit mapping'
              : 'Disabled'}
          </dd>
          <dt>Paid native models</dt>
          <dd>
            {Object.keys(route.paid_targets ?? {}).join(', ') || 'None'}
          </dd>
          <dt>Draft models</dt>
          <dd>{route.models.join(', ')}</dd>
          <dt>Draft pool</dt>
          <dd>{route.pool_id}</dd>
          <dt>Draft Codex CLI identity compatibility</dt>
          <dd>
            {route.codex_cli_identity
              ? 'Enabled, requires publication'
              : 'Disabled'}
          </dd>
          <dt>Draft enabled</dt>
          <dd>{route.enabled ? 'Yes' : 'No'}</dd>
          <dt>Latest publication</dt>
          <dd>
            {route.published_at
              ? `Revision ${route.revision} · ${formatDate(route.published_at)}`
              : 'Not published'}
          </dd>
        </dl>
        {#if detail === route.id}
          <RouteRevisions id={route.id} />
          {#if loadClientConfiguration && route.published_at}
            <ClientConfiguration
              {route}
              {gatewayURL}
              load={loadClientConfiguration}
            />
          {:else if !route.published_at}
            <p>Publish this route before generating client configuration.</p>
          {:else}
            <p>
              Client configuration requires the integrated qualified adapter. No
              client release or local workflow is claimed by this management
              view.
            </p>
          {/if}
        {/if}
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, routes.data?.nextCursor)}
    label="routes pages"
  />
</div>

<style>
  .section {
    display: grid;
    gap: 1rem;
  }
  .actions {
    display: flex;
    gap: 0.65rem;
    align-items: center;
    flex-wrap: wrap;
  }
  h2 {
    flex: 1;
    font-size: 1.25rem;
    font-weight: 600;
  }
  h3 {
    flex: 1;
    font-weight: 600;
  }
  article {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dt {
    font-weight: 500;
  }
  dd,
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }
  .badges {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
  }
  .empty {
    padding: 2rem;
    border: 1px dashed var(--border);
  }
  .field-error {
    color: var(--danger);
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
