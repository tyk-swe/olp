<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listCodeAccounts,
    listCodePools,
    listCodeRoutes,
    listCodeBudgets,
    publishCodeRoute,
    type CodeRoute
  } from '#lib/features/code-mode/api.js';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import { mutationError, type CodeEditing } from './presentation';
  import {
    listProviders,
    listCredentialSlots
  } from '#lib/features/providers/index.js';
  import ResourceEditor from './ResourceEditor.svelte';
  import CodeAccountList from './CodeAccountList.svelte';
  import CodeBudgetList from './CodeBudgetList.svelte';
  import CodePoolList from './CodePoolList.svelte';
  import CodeRouteList from './CodeRouteList.svelte';
  import { managementSections, type CodeManagementKind } from './sections';

  let {
    projectId,
    kind,
    allowed,
    gatewayURL,
    onDraftChange
  }: {
    projectId: string;
    kind: CodeManagementKind;
    allowed: boolean;
    gatewayURL: string;
    onDraftChange: (dirty: boolean) => void;
  } = $props();
  const section = $derived(managementSections[kind]);
  let editing = $state<CodeEditing | null>(null);
  let paging = $state(emptyCursorHistory());
  const publishMutation = useMutation();
  let error = $state('');
  let notice = $state('');
  let detail = $state('');
  const client = useQueryClient();
  const filters = $derived({ project_id: projectId, cursor: paging.cursor });
  const accounts = createQuery(() => ({
    queryKey: codeKeys.collection('accounts', filters),
    queryFn: ({ signal }) => listCodeAccounts(filters, signal),
    enabled: kind === 'accounts'
  }));
  const pools = createQuery(() => ({
    queryKey: codeKeys.collection('pools', filters),
    queryFn: ({ signal }) => listCodePools(filters, signal),
    enabled: kind === 'pools'
  }));
  const routes = createQuery(() => ({
    queryKey: codeKeys.collection('routes', filters),
    queryFn: ({ signal }) => listCodeRoutes(filters, signal),
    enabled: kind === 'routes'
  }));
  const budgets = createQuery(() => ({
    queryKey: codeKeys.collection('budgets', filters),
    queryFn: ({ signal }) => listCodeBudgets(filters, signal),
    enabled: kind === 'budgets'
  }));
  const selected = $derived({ accounts, pools, routes, budgets }[kind]);

  const inventory = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, kind),
    queryFn: async ({ signal }) => ({
      providers:
        kind === 'routes'
          ? await Promise.all(
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
                  ).items.filter(
                    (slot) => slot.enabled && slot.credential_version_id
                  )
                }))
            )
          : [],
      accounts:
        kind === 'pools' || kind === 'routes'
          ? await collectCursorPages((cursor) =>
              listCodeAccounts({ project_id: projectId, cursor }, signal)
            )
          : [],
      pools:
        kind === 'routes'
          ? await collectCursorPages((cursor) =>
              listCodePools({ project_id: projectId, cursor }, signal)
            )
          : [],
      routes:
        kind === 'budgets'
          ? await collectCursorPages((cursor) =>
              listCodeRoutes({ project_id: projectId, cursor }, signal)
            )
          : []
    }),
    enabled: !!editing
  }));
  async function saved() {
    editing = null;
    onDraftChange(false);
    notice =
      kind === 'routes'
        ? 'Draft saved. Publish to change serving configuration.'
        : 'Changes saved.';
    const refreshed = await readResult(() =>
      client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      )
    );
    if (!refreshed.ok)
      error = `Changes saved, but refreshing failed. Use Refresh to reload. ${errorMessage(refreshed.error)}`;
  }
  async function publish(route: CodeRoute) {
    if (
      publishMutation.busy ||
      !allowed ||
      !(await confirmAction({
        title: `Publish ${route.slug}?`,
        body: `Provider connections are captured without a test request.${route.codex_cli_identity ? ' Codex CLI identity compatibility fills Originator=codex_cli_rs only when every supplied Originator value is absent or blank on Codex subscription HTTP and WebSocket requests. Explicit identity headers, User-Agent, prompts, tools, reasoning and state are kept. This may break the provider’s terms and risk account suspension.' : ' Codex CLI identity compatibility is off. Identity headers and prompts are kept.'}`,
        confirmLabel: 'Publish'
      }))
    )
      return;
    error = '';
    notice = '';
    await publishMutation.submit(
      { id: route.id, etag: route.etag },
      (submission, requestId) => publishCodeRoute(submission, requestId),
      async (_, current) => {
        notice =
          'Route published without synthetic inference. Serving health and qualification remain separate.';
        await client.invalidateQueries(
          { queryKey: codeKeys.root },
          { throwOnError: true }
        );
        if (!current()) return;
      },
      {
        key: route.id,
        failure: (e, stage) =>
          stage === 'accept'
            ? `Route published, but refreshing failed. ${errorMessage(e)}`
            : mutationError(e)
      }
    );
    error = publishMutation.error;
  }
</script>

<div class="management">
  <div class="actions">
    <h2>{section.title}</h2>
    {#if allowed}<button
        type="button"
        class="button button-primary"
        disabled={!!editing || publishMutation.busy}
        onclick={() => (editing = { kind })}>Create {section.noun}</button
      >{/if}
    <button
      type="button"
      class="button button-secondary"
      disabled={publishMutation.busy || selected.isFetching}
      onclick={() => selected.refetch()}>Refresh</button
    >
  </div>
  {#if !allowed}<ReadOnlyNote>
      Read only. Changes require configure permission and project manager or
      global authority.
    </ReadOnlyNote>{/if}
  {#if error}<p role="alert" class="field-error">{error}</p>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  {#if editing}
    {#if inventory.isError}<LoadError
        error={inventory.error}
        onRetry={() => inventory.refetch()}
        retryLabel="Retry choices"
      >
        <button
          class="text-button"
          type="button"
          onclick={() => (editing = null)}>Cancel</button
        >
      </LoadError>
    {:else if inventory.data}
      {#key editing}<ResourceEditor
          {editing}
          {projectId}
          {allowed}
          {onDraftChange}
          {...inventory.data}
          onSaved={saved}
          onCancel={() => (editing = null)}
        />{/key}
    {:else}<p role="status">Loading editor choices…</p>{/if}
  {/if}
  {#if selected.isPending}<p role="status">Loading {kind}…</p>
  {:else if selected.isError}<LoadError
      error={selected.error}
      onRetry={() => selected.refetch()}
    />
  {:else if !selected.data?.items.length}<p class="empty">
      No {section.empty} in this project.
    </p>
  {:else}
    {#if kind === 'accounts'}
      <CodeAccountList
        items={accounts.data?.items ?? []}
        {allowed}
        busy={!!editing || publishMutation.busy}
        onEdit={(account) => (editing = { kind: 'accounts', current: account })}
      />
    {:else if kind === 'pools'}
      <CodePoolList
        items={pools.data?.items ?? []}
        {allowed}
        busy={!!editing}
        onEdit={(pool) => (editing = { kind: 'pools', current: pool })}
      />
    {:else if kind === 'routes'}
      <CodeRouteList
        items={routes.data?.items ?? []}
        {allowed}
        busy={!!editing || publishMutation.busy}
        bind:detail
        {gatewayURL}
        onEdit={(route) => (editing = { kind: 'routes', current: route })}
        onPublish={publish}
      />
    {:else}
      <CodeBudgetList
        items={budgets.data?.items ?? []}
        {allowed}
        busy={!!editing}
        onEdit={(budget) => (editing = { kind: 'budgets', current: budget })}
      />
    {/if}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, selected.data?.nextCursor)}
    label={`${kind} pages`}
  />
</div>

<style>
  .management {
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
  .empty {
    padding: 2rem;
    border: 1px dashed var(--border);
  }
</style>
