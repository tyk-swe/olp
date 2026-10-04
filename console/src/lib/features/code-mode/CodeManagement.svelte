<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listCodeAccounts,
    listCodePools,
    listCodeRoutes,
    listCodeBudgets,
    publishCodeRoute,
    type CodeRoute,
    type CodeClientConfiguration
  } from '$lib/api/code-mode';
  import { collectCursorPages } from '$lib/api/pagination';
  import { errorMessage } from '$lib/api/http';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatDate } from '$lib/format';
  import { codeKeys } from './codeKeys';
  import { mutationError, tokenCount, type CodeEditing } from './presentation';
  import ResourceEditor from './ResourceEditor.svelte';
  import RouteRevisions from './RouteRevisions.svelte';
  import ClientConfiguration from './ClientConfiguration.svelte';

  let {
    projectId,
    kind,
    allowed,
    gatewayURL,
    loadClientConfiguration
  }: {
    projectId: string;
    kind: 'accounts' | 'pools' | 'routes' | 'budgets';
    allowed: boolean;
    gatewayURL: string;
    loadClientConfiguration?: (
      route: CodeRoute,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();
  const sections = {
    accounts: { title: 'Subscription accounts', noun: 'account' },
    pools: { title: 'Account pools', noun: 'pool' },
    routes: { title: 'Native-model routes', noun: 'route' },
    budgets: { title: 'Optional hard token budgets', noun: 'budget' }
  };
  const section = $derived(sections[kind]);
  let editing = $state<CodeEditing | null>(null);
  let paging = $state(emptyCursorHistory());
  let busy = $state(false);
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
  // Each editor chooses references from at most one other collection: pools
  // assign accounts, routes select a pool and budgets may scope to a route.
  const inventory = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, kind),
    queryFn: async ({ signal }) => ({
      accounts:
        kind === 'pools'
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
    notice =
      kind === 'routes'
        ? 'Draft saved. Publish to change serving configuration.'
        : 'Changes saved.';
    await client.invalidateQueries({ queryKey: codeKeys.root });
  }
  async function publish(route: CodeRoute) {
    if (
      busy ||
      !allowed ||
      !confirm(
        `Publish the current draft for ${route.slug}? Provider connections will be captured without an inference probe.`
      )
    )
      return;
    busy = true;
    error = '';
    notice = '';
    try {
      await publishCodeRoute(route);
      notice =
        'Route published without synthetic inference. Serving health and qualification remain separate.';
      await client.invalidateQueries({ queryKey: codeKeys.root });
    } catch (e) {
      error = mutationError(e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="management">
  <div class="actions">
    <h2>{section.title}</h2>
    {#if allowed}<button
        type="button"
        class="button button-primary"
        disabled={!!editing || busy}
        onclick={() => (editing = { kind })}>Create {section.noun}</button
      >{/if}
    <button
      type="button"
      class="button button-secondary"
      disabled={busy || selected.isFetching}
      onclick={() => selected.refetch()}>Refresh</button
    >
  </div>
  {#if !allowed}<ReadOnlyNote>
      Read only. Changes require configure permission and project manager or
      global authority.
    </ReadOnlyNote>{/if}
  {#if error}<p role="alert" class="field-error">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if editing}
    {#if inventory.isError}<p role="alert">
        {errorMessage(inventory.error)}
        <button
          class="text-button"
          type="button"
          onclick={() => inventory.refetch()}>Retry choices</button
        >
        <button
          class="text-button"
          type="button"
          onclick={() => (editing = null)}>Cancel</button
        >
      </p>
    {:else if inventory.data}
      {#key editing}<ResourceEditor
          {editing}
          {projectId}
          {allowed}
          {...inventory.data}
          onSaved={saved}
          onCancel={() => (editing = null)}
        />{/key}
    {:else}<p role="status">Loading editor choices…</p>{/if}
  {/if}
  {#if selected.isPending}<p role="status">Loading {kind}…</p>
  {:else if selected.isError}<p role="alert">
      {errorMessage(selected.error)}
      <button
        class="text-button"
        type="button"
        onclick={() => selected.refetch()}>Retry</button
      >
    </p>
  {:else if !selected.data?.items.length}<p class="empty">
      No {kind} in this project.
    </p>
  {:else}
    {#if kind === 'accounts'}
      <p>
        Eligibility reflects current grant authority. Unknown serving health
        means no qualified observation; enrollment is not an inference test.
        Provider allowance is separate from local budgets and reported usage.
      </p>
      {#each accounts.data?.items ?? [] as account (account.id)}
        <article class="card">
          <div class="actions">
            <h3>{account.name}</h3>
            {#if allowed}<button
                class="button button-secondary"
                type="button"
                disabled={!!editing}
                onclick={() =>
                  (editing = { kind: 'accounts', current: account })}
                >Edit account</button
              >{/if}
          </div>
          <dl>
            <dt>Account / principal</dt>
            <dd>{account.id} / {account.principal}</dd>
            <dt>Eligibility</dt>
            <dd>
              {account.eligible ? 'Eligible' : 'Ineligible'} · {account.enabled
                ? 'enabled'
                : 'disabled'}
            </dd>
            <dt>Grant state</dt>
            <dd>{account.grant_state}</dd>
            <dt>Serving health</dt>
            <dd>{account.health}</dd>
            <dt>Native models</dt>
            <dd>{account.models.join(', ')}</dd>
            <dt>Provider / credential</dt>
            <dd>{account.provider_id} / {account.credential_id}</dd>
            <dt>Provider-reported allowance</dt>
            <dd>
              {#if account.allowance}
                {#each [{ label: 'Tokens', value: account.allowance.remaining_tokens, observation: account.allowance.token_observation ?? account.allowance }, { label: 'Requests', value: account.allowance.remaining_requests, observation: account.allowance.request_observation ?? account.allowance }] as count (count.label)}
                  <p>
                    {count.label}: {tokenCount(count.value)}
                    {#if count.value != null}
                      · Observed {formatDate(count.observation.observed_at)}
                      · Reset {count.observation.resets_at
                        ? formatDate(count.observation.resets_at)
                        : 'Unknown'}
                    {/if}
                  </p>
                {/each}
                {#if account.allowance.windows?.length}
                  {#each account.allowance.windows as window (window.limit_id + ':' + window.window)}
                    <p>
                      {window.limit_id} / {window.window}: {window.used_percent}%
                      used · {window.remaining_percent}% remaining<br />
                      Window: {window.window_minutes == null
                        ? 'Unknown'
                        : window.window_minutes + ' minutes'} · Reset {window.resets_at
                        ? formatDate(window.resets_at)
                        : 'Unknown'}<br />
                      Observed {formatDate(window.observed_at)}
                    </p>
                  {/each}
                {:else}Percent: {account.allowance.remaining_percent == null
                    ? 'Unknown'
                    : account.allowance.remaining_percent + '%'}<br />Observed {formatDate(
                    account.allowance.observed_at
                  )} · Reset {account.allowance.resets_at
                    ? formatDate(account.allowance.resets_at)
                    : 'Unknown'}{/if}
              {:else}Unknown — no provider observation{/if}
            </dd>
            {#if account.allowance?.credits}
              <dt>Provider-reported credits</dt>
              <dd>
                Has credits: {account.allowance.credits.has_credits
                  ? 'Yes'
                  : 'No'} · Unlimited: {account.allowance.credits.unlimited
                  ? 'Yes'
                  : 'No'} · Balance: {account.allowance.credits.balance ??
                  'Unknown'}<br />
                Observed {formatDate(account.allowance.credits.observed_at)}.
                Credits do not override exhausted subscription windows.
              </dd>
            {/if}
          </dl>
        </article>
      {/each}
    {:else if kind === 'pools'}
      {#each pools.data?.items ?? [] as pool (pool.id)}
        <article class="card">
          <div class="actions">
            <h3>{pool.name}</h3>
            {#if allowed}<button
                class="button button-secondary"
                type="button"
                disabled={!!editing}
                onclick={() => (editing = { kind: 'pools', current: pool })}
                >Edit pool</button
              >{/if}
          </div>
          <dl>
            <dt>Pool</dt>
            <dd>{pool.id} · {pool.kind}</dd>
            <dt>Owner</dt>
            <dd>{pool.owner_user_id ?? 'Shared project pool'}</dd>
            <dt>Assigned accounts</dt>
            <dd>{pool.account_ids.join(', ') || 'None'}</dd>
            <dt>Explicit key permissions</dt>
            <dd>
              {pool.api_key_ids.join(', ') || 'None — no key can use this pool'}
            </dd>
          </dl>
        </article>
      {/each}
    {:else if kind === 'routes'}
      <p>
        Route base paths select policy; the client sends native models
        unchanged. Connection edits require route republish. Pool/account/key
        permissions remain live.
      </p>
      {#each routes.data?.items ?? [] as route (route.id)}
        <article class="card">
          <div class="actions">
            <h3>{route.slug}</h3>
            {#if allowed}<button
                class="button button-secondary"
                type="button"
                disabled={!!editing || busy}
                onclick={() => (editing = { kind: 'routes', current: route })}
                >Edit draft</button
              ><button
                class="button button-primary"
                type="button"
                disabled={!!editing || busy}
                onclick={() => publish(route)}>Publish route</button
              >{/if}<button
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
            <dt>Draft models</dt>
            <dd>{route.models.join(', ')}</dd>
            <dt>Draft pool</dt>
            <dd>{route.pool_id}</dd>
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
            {#if loadClientConfiguration && route.published_at}<ClientConfiguration
                {route}
                {gatewayURL}
                load={loadClientConfiguration}
              />
            {:else if !route.published_at}<p>
                Publish this route before generating client configuration.
              </p>
            {:else}<p>
                Client configuration requires the integrated qualified adapter.
                No client release or local workflow is claimed by this
                management view.
              </p>{/if}
          {/if}
        </article>
      {/each}
    {:else}
      <p>
        Daily and monthly periods use UTC. These are token caps, never
        subscription allowance or billed spend. Existing key
        request/rate/concurrency limits also apply. Code-mode does not enforce
        monetary estimates.
      </p>
      {#each budgets.data?.items ?? [] as budget (budget.id)}
        <article class="card">
          <div class="actions">
            <h3>Token budget</h3>
            {#if allowed}<button
                class="button button-secondary"
                type="button"
                disabled={!!editing}
                onclick={() => (editing = { kind: 'budgets', current: budget })}
                >Edit budget</button
              >{/if}
          </div>
          <dl>
            <dt>Budget</dt>
            <dd>{budget.id} · {budget.enabled ? 'enabled' : 'disabled'}</dd>
            <dt>Route / key scope</dt>
            <dd>
              {budget.route_id ?? 'All project routes'} / {budget.api_key_id ??
                'All project keys'}
            </dd>
            <dt>Daily tokens</dt>
            <dd>
              {budget.daily_tokens == null
                ? 'No cap'
                : tokenCount(budget.daily_tokens)}
            </dd>
            <dt>Monthly tokens</dt>
            <dd>
              {budget.monthly_tokens == null
                ? 'No cap'
                : tokenCount(budget.monthly_tokens)}
            </dd>
          </dl>
        </article>
      {/each}
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
