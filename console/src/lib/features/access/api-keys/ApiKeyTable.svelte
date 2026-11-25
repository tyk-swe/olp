<script lang="ts">
  import { resolve } from '$app/paths';
  import { formatBudget, formatDate } from '#lib/format.js';
  import {
    budgetState,
    budgetStateLabel,
    currentBudgetLimit
  } from '#lib/features/access/api-keys/budgetPresentation.js';
  import {
    isApiKeyExpired,
    isApiKeyUsable
  } from '#lib/features/access/api-keys/apiKeyLifecycle.js';
  import type { ApiKey } from '#lib/features/access/api-keys/api.js';

  let {
    items,
    canManage,
    limitsEnforced,
    gatewayAvailable,
    busy,
    busyKey,
    onRotate,
    onPause,
    onResume,
    onRevoke
  }: {
    items: ApiKey[];
    canManage: boolean;
    limitsEnforced: boolean;
    gatewayAvailable: boolean;
    busy: boolean;
    busyKey: string;
    onRotate: (key: ApiKey) => void;
    onPause: (key: ApiKey) => void;
    onResume: (key: ApiKey) => void;
    onRevoke: (key: ApiKey) => void;
  } = $props();
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
<div
  class="table-shell key-table"
  tabindex="0"
  role="region"
  aria-label="API keys"
>
  <table class="data-table">
    <thead>
      <tr>
        <th>Name / lookup ID</th><th>Status</th><th>Scope</th><th>
          Limits
        </th><th>Budget</th><th>Creator / created</th><th>
          <span class="sr-only">Actions</span>
        </th>
      </tr>
    </thead>
    <tbody>
      {#each items as key (key.id)}
        {@const budget = budgetState(key.budget)}
        {@const rates = key.effective_limits ?? key}
        {@const expired = isApiKeyExpired(key)}
        {@const usable = isApiKeyUsable(key)}
        <tr>
          <td>
            <strong>{key.name}</strong><br /><code>{key.lookup_id}</code><br
            /><small>{key.project_name ?? 'Installation-wide'}</small>
          </td>
          <td>
            <span
              class:danger={Boolean(key.revoked_at)}
              class:warning={expired || Boolean(key.paused_at)}
              class:success={usable && !key.paused_at}
              class="badge"
            >
              {key.revoked_at
                ? 'revoked'
                : expired
                  ? 'expired'
                  : key.paused_at
                    ? 'paused'
                    : 'active'}
            </span><br /><small>
              {key.expires_at
                ? `Expires ${formatDate(key.expires_at)}`
                : 'No expiry'}
            </small><br /><small>
              {key.rotated_at
                ? `Rotated ${formatDate(key.rotated_at)}`
                : 'Never rotated'}
            </small>
            {#if key.rotation_due_at}<br /><small>
                Rotation due {formatDate(key.rotation_due_at)}
              </small>{/if}
            {#each key.active_overlaps ?? [] as overlap (overlap.lookup_id)}<br
              /><small>
                Previous {overlap.lookup_id} valid until {formatDate(
                  overlap.expires_at
                )}
              </small>{/each}
          </td>
          <td>
            {key.scopes.join(', ') || 'none'}<br />
            {#if key.allowed_routes.length}<small>
                {key.allowed_routes.join(', ')}
              </small>{/if}
            {#if key.allowed_route_groups?.length}{#if key.allowed_routes.length}<br
                />{/if}<small>
                Groups: {key.allowed_route_groups.join(', ')}
              </small>
            {:else if !key.allowed_routes.length}<small>all routes</small>{/if}
          </td>
          <td>
            <small>
              {rates.requests_per_minute
                ? `${rates.requests_per_minute} RPM`
                : 'unlimited RPM'}<br />{rates.tokens_per_minute
                ? `${rates.tokens_per_minute} TPM`
                : 'unlimited TPM'} · {rates.max_concurrency
                ? `${rates.max_concurrency} concurrent`
                : 'unlimited concurrency'}
            </small>
          </td>
          <td>
            {#if !limitsEnforced || budget === 'policy'}<small>
                Saved policy · not enforced
              </small>
            {:else if currentBudgetLimit(key.budget.daily) !== null || currentBudgetLimit(key.budget.monthly) !== null || currentBudgetLimit(key.budget.weekly) != null}
              <small>
                {#if currentBudgetLimit(key.budget.daily) !== null}Daily {formatBudget(
                    key.budget.daily.accrued
                  )} / {formatBudget(currentBudgetLimit(key.budget.daily))}{/if}
                {#if key.budget.weekly && currentBudgetLimit(key.budget.weekly) != null}{#if currentBudgetLimit(key.budget.daily) !== null}<br
                    />{/if}Weekly {formatBudget(key.budget.weekly.accrued)} / {formatBudget(
                    currentBudgetLimit(key.budget.weekly)
                  )}{/if}
                {#if (currentBudgetLimit(key.budget.daily) !== null || currentBudgetLimit(key.budget.weekly) != null) && currentBudgetLimit(key.budget.monthly) !== null}<br
                  />{/if}
                {#if currentBudgetLimit(key.budget.monthly) !== null}Monthly {formatBudget(
                    key.budget.monthly.accrued
                  )} / {formatBudget(
                    currentBudgetLimit(key.budget.monthly)
                  )}{/if}
              </small>
              {#if budget === 'exhausted' || budget === 'unknown'}<br /><span
                  class="badge"
                  class:danger={budget === 'exhausted'}
                  class:warning={budget === 'unknown'}
                >
                  {budgetStateLabel(budget)}
                </span>{/if}
            {:else}
              <small>No cost budget</small>
            {/if}
          </td>
          <td>
            <a
              href={`${resolve('keys')}?created_by=${key.created_by}`}
              aria-label={`Review API keys issued by ${key.created_by_email}`}
            >
              {key.created_by_email}
            </a><br /><small>{formatDate(key.created_at)}</small>
          </td>
          <td>
            <div class="row-actions">
              {#if gatewayAvailable}<a
                  class="button button-secondary"
                  href={resolve(`usage?api_key_id=${key.id}`)}
                  aria-label={`Usage for ${key.name}`}
                >
                  Usage
                </a>
              {/if}

              <a
                class="button button-secondary"
                href={resolve(`keys/${key.id}`)}
              >
                {canManage && usable && !key.workload_issuer_id
                  ? 'Edit'
                  : 'View'}
              </a>{#if canManage && usable}{#if key.paused_at}<button
                    class="button button-secondary"
                    type="button"
                    onclick={() => onResume(key)}
                    disabled={busy}
                  >
                    {busyKey === `resume-${key.id}` ? 'Resuming…' : 'Resume'}
                  </button>{:else}<button
                    class="button button-secondary"
                    type="button"
                    onclick={() => onPause(key)}
                    disabled={busy}
                  >
                    {busyKey === `pause-${key.id}` ? 'Pausing…' : 'Pause'}
                  </button>{/if}{/if}{#if canManage && !key.revoked_at}{#if usable && !key.workload_issuer_id}<button
                    class="button button-secondary"
                    type="button"
                    onclick={() => onRotate(key)}
                    disabled={busy}
                  >
                    {busyKey === `rotate-${key.id}` ? 'Rotating…' : 'Rotate'}
                  </button>{/if}<button
                  class="button button-secondary button-danger"
                  type="button"
                  onclick={() => onRevoke(key)}
                  disabled={busy}
                >
                  {busyKey === `revoke-${key.id}` ? 'Revoking…' : 'Revoke'}
                </button>{/if}
            </div>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<style>
  .key-table {
    margin-top: 1.5rem;
  }
  code {
    font-size: var(--text-caption);
  }
  td small {
    color: var(--foreground-muted);
  }
  .row-actions {
    display: flex;
    gap: 0.4rem;
  }
</style>
