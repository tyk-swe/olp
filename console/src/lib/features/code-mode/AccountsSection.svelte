<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { listCodeAccounts, type CodeAccount } from './api.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import { formatDate } from '#lib/format.js';
  import AccountEditor from './AccountEditor.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import {
    adapterLabel,
    allowanceEvidenceLabel,
    allowanceWindowSummary,
    cooldownSummary,
    extraUsageLabel,
    servingHealthLabel,
    showsExtraUsage,
    tokenCount
  } from './presentation';
  import { managementSections } from './sections';

  let {
    projectId,
    allowed,
    onDraftChange
  }: {
    projectId: string;
    allowed: boolean;
    onDraftChange: (dirty: boolean) => void;
  } = $props();

  const section = managementSections.accounts;
  const client = useQueryClient();
  let editing = $state<{ current?: CodeAccount } | null>(null);
  let paging = $state(emptyCursorHistory());
  let error = $state('');
  let notice = $state('');
  const filters = $derived({ project_id: projectId, cursor: paging.cursor });
  const accounts = createQuery(() => ({
    queryKey: codeKeys.collection('accounts', filters),
    queryFn: ({ signal }) => listCodeAccounts(filters, signal)
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
    notice = 'Changes saved.';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Changes saved, but refreshing failed. Use Refresh to reload. ${errorMessage(e)}`;
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
        disabled={!!editing}
        onclick={() => (editing = {})}>Create {section.noun}</button
      >
    {/if}
    <button
      class="button button-secondary"
      type="button"
      onclick={refresh}
      disabled={accounts.isFetching}>Refresh</button
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
    {#key editing}
      <AccountEditor
        current={editing.current}
        {projectId}
        {allowed}
        {onDraftChange}
        {onSaved}
        onCancel={() => (editing = null)}
      />
    {/key}
  {/if}
  {#if accounts.isPending}
    <p role="status">Loading accounts…</p>
  {:else if accounts.isError}
    <LoadError error={accounts.error} onRetry={() => accounts.refetch()} />
  {:else if !accounts.data?.items.length}
    <p class="empty">No {section.empty} in this project.</p>
  {:else}
    <p>
      Eligibility depends on grant authority, serving cooldowns and observed
      allowance. Unknown serving health means no qualified observation.
      Enrollment is not an inference test. A rejected refresh lapses the grant
      and requires enrollment again. Passing a retry or reset time permits a new
      attempt without changing the reported allowance. Provider allowance is
      separate from local budgets and reported usage
    </p>
    {#each accounts.data.items as account (account.id)}
      <article class="card">
        <div class="actions">
          <h3>{account.name}</h3>
          {#if allowed}
            <button
              class="button button-secondary"
              type="button"
              disabled={!!editing}
              onclick={() => (editing = { current: account })}
              >Edit account</button
            >
          {/if}
        </div>
        <dl>
          <dt>Subscription</dt>
          <dd><span class="badge">{adapterLabel(account.adapter)}</span></dd>
          {#if showsExtraUsage(account.adapter)}
            <dt>Extra Usage</dt>
            <dd>{extraUsageLabel(account.extra_usage_enabled)}</dd>
          {/if}
          <dt>Ordering signal</dt>
          <dd>
            No qualified model-to-limit mapping. Legacy eligibility and weighted
            selection apply.
          </dd>
          <dt>Account / principal</dt>
          <dd>{account.id} / {account.principal}</dd>
          <dt>Eligibility</dt>
          <dd>
            {account.eligible ? 'Eligible' : 'Ineligible'} · {account.enabled
              ? 'enabled'
              : 'disabled'}
          </dd>
          <dt>Grant state</dt>
          <dd>
            {account.grant_state === 'lapsed'
              ? 'Grant lapsed, enroll again'
              : account.grant_state}
          </dd>
          <dt>Serving health</dt>
          <dd>
            {servingHealthLabel(account.health)}
            {#if account.health_observed_at}
              · Observed {formatDate(account.health_observed_at)}
            {/if}
          </dd>
          <dt>Serving retry time</dt>
          <dd>
            {account.unavailable_until
              ? formatDate(account.unavailable_until)
              : 'No serving cooldown reported'}
            {#if account.cooldowns?.length}
              {#each account.cooldowns as cooldown (`${cooldown.class}:${cooldown.model ?? ''}`)}
                <p>
                  {cooldownSummary(cooldown)} · until {formatDate(
                    cooldown.retry_at
                  )}
                </p>
              {/each}
            {/if}
          </dd>
          {#if account.cycle_anchor}
            <dt>Subscription cycle</dt>
            <dd>
              Anchor {account.cycle_anchor} · Monthly reset {account.next_monthly_reset ??
                'Unknown'}
            </dd>
          {/if}
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
                {#each account.allowance.windows as window (`${window.limit_id}:${window.window}`)}
                  <p>
                    {window.limit_id} / {window.window}: {allowanceWindowSummary(
                      window
                    )}<br />
                    Window: {window.window_minutes == null
                      ? 'Unknown'
                      : `${window.window_minutes} minutes`} · Reset {window.resets_at
                      ? formatDate(window.resets_at)
                      : 'Unknown'}<br />
                    {allowanceEvidenceLabel(window.evidence)} · Observed {formatDate(
                      window.observed_at
                    )}
                  </p>
                {/each}
              {:else}
                Percent: {account.allowance.remaining_percent == null
                  ? 'Unknown'
                  : `${account.allowance.remaining_percent}%`}<br />
                Observed {formatDate(account.allowance.observed_at)} · Reset {account
                  .allowance.resets_at
                  ? formatDate(account.allowance.resets_at)
                  : 'Unknown'}
              {/if}
            {:else}
              Unknown — no provider observation
            {/if}
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
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, accounts.data?.nextCursor)}
    label="accounts pages"
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
