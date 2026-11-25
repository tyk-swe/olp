<script lang="ts">
  import { resolve } from '$app/paths';
  import type { ConfigurationPlan, ConfigurationPlanItem } from './api.js';

  let {
    plan,
    actions,
    enrollments,
    requiredSecrets,
    secrets = $bindable(),
    busy,
    applyResult,
    hasCode,
    apply,
    itemLabel,
    pinnedBy
  }: {
    plan: ConfigurationPlan;
    actions: ConfigurationPlanItem[];
    enrollments: string[];
    requiredSecrets: string[];
    secrets: Record<string, string>;
    busy: boolean;
    applyResult: string;
    hasCode: boolean;
    apply: () => void;
    itemLabel: (item: ConfigurationPlanItem) => string;
    pinnedBy: (digest: string) => string;
  } = $props();
</script>

{#snippet unavailablePlugin(item: ConfigurationPlanItem)}
  Plugin build <span class="mono">{item.key}</span> (pinned by {pinnedBy(
    item.key
  )})
  {#if item.detail === 'plugin_unconfined_disabled'}
    is unconfined, and this deployment does not enable unconfined plugins: an
    operator enables that tier, then plan again.
  {:else}
    {@const approval = item.detail === 'plugin_not_approved'}
    {approval ? 'awaits approval' : 'is not installed here'}: {approval
      ? 'an owner approves it'
      : 'install and approve it'} on the
    <a href={resolve('plugins')}>Plugins page</a>
    , then plan again.
  {/if}
{/snippet}

<div class="card" data-testid="promotion-plan">
  <div class="card" data-testid="promotion-plan">
    <small class="mono">Digest {plan.digest}</small>
    {#if actions.length}
      <h3>Actions</h3>
      <ul data-testid="plan-actions">
        {#each actions as item (item.kind + item.key + item.action)}<li>
            {itemLabel(item)}
          </li>{/each}
      </ul>
    {/if}
    {#if enrollments.length}
      <h3>Grant enrollment</h3>
      <p>
        Exports never carry grants. After applying, enroll a grant for each of
        these credential slots; their providers activate only once the slots
        they serve with hold one.
      </p>
      <ul data-testid="plan-grant-enrollments">
        {#each enrollments as ref (ref)}<li class="mono">{ref}</li>{/each}
      </ul>
    {/if}
    {#if plan.conflicts.length}
      <h3>Conflicts</h3>
      <ul data-testid="plan-conflicts" class="promotion-problems">
        {#each plan.conflicts as item (item.kind + item.key)}<li>
            {itemLabel(item)}
          </li>{/each}
      </ul>
    {/if}
    {#if plan.blockers.length}
      <h3>Blockers</h3>
      <ul data-testid="plan-blockers" class="promotion-problems">
        {#each plan.blockers as item (item.kind + item.key)}<li>
            {#if item.kind === 'plugin'}{@render unavailablePlugin(
                item
              )}{:else}{itemLabel(item)}{/if}
          </li>{/each}
      </ul>
    {/if}
    {#each requiredSecrets as ref (ref)}
      <label for={`secret-${ref}`}>
        Credential <span class="mono">{ref}</span>
      </label>
      <input
        id={`secret-${ref}`}
        type="password"
        autocomplete="off"
        bind:value={secrets[ref]}
      />
    {/each}
    {#if plan.blockers.length === 0 && plan.conflicts.length === 0}
      <button class="button" onclick={apply} disabled={busy}>
        {busy ? 'Applying…' : 'Apply'}
      </button>
    {/if}
    {#if applyResult === 'staged'}<p role="status" data-testid="apply-result">
        {hasCode ? 'Configuration applied.' : 'Configuration staged.'}
      </p>{/if}
  </div>
</div>
