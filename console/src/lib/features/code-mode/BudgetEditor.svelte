<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    saveCodeBudget,
    type CodeBudget,
    type CodeRoute,
    type CodeBudgetWrite
  } from '$lib/api/code-mode';
  import { tokenLimit } from './presentation';

  import {
    useResourceSubmission,
    type ResourceEditorProps
  } from './resourceSubmission.svelte';
  import ResourceEditorForm from './ResourceEditorForm.svelte';

  let {
    current,
    projectId,
    routes,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: ResourceEditorProps<CodeBudget> & { routes: CodeRoute[] } = $props();
  const access = useRole();
  const initial = untrack(() => current);
  let routeId = $state(initial?.route_id ?? '');
  let keyId = $state(initial?.api_key_id ?? '');
  let daily = $state(initial?.daily_tokens?.toString() ?? '');
  let monthly = $state(initial?.monthly_tokens?.toString() ?? '');
  let enabled = $state(initial?.enabled ?? true);

  const keys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal),
    enabled: access.can('api_keys.read')
  }));
  const projectKeysList = $derived(
    (keys.data ?? []).filter((key) => key.project_id === projectId)
  );

  const submission = useResourceSubmission({
    allowed: () => allowed,
    creating: () => !current,
    onDraftChange: (dirty) => onDraftChange(dirty),
    onSaved: () => onSaved(),
    onCancel: () => onCancel(),
    build: (): CodeBudgetWrite => {
      const dailyTokens = tokenLimit(daily),
        monthlyTokens = tokenLimit(monthly);
      if (dailyTokens === null && monthlyTokens === null)
        throw new Error('Set at least one daily or monthly token limit.');
      return {
        project_id: projectId,
        route_id: routeId || null,
        api_key_id: keyId || null,
        daily_tokens: dailyTokens,
        monthly_tokens: monthlyTokens,
        enabled
      };
    },
    persist: (input, requestId) => saveCodeBudget(input, current, requestId)
  });
</script>

<ResourceEditorForm
  title="token budget"
  creating={!current}
  {allowed}
  {submission}
  referenceError={keys.error}
>
  <label for="code-budget-route">Route scope</label><select
    id="code-budget-route"
    bind:value={routeId}
    disabled={!!current}
    ><option value="">All project routes</option
    >{#each routes as route (route.id)}<option value={route.id}
        >{route.slug}</option
      >{/each}
    {#if routeId && !routes.some((r) => r.id === routeId)}
      <option value={routeId}>{routeId} · current selection</option>
    {/if}</select
  >
  <label for="code-budget-key">Key scope</label><select
    id="code-budget-key"
    bind:value={keyId}
    disabled={!!current}
    ><option value="">All project keys</option
    >{#each projectKeysList as key (key.id)}<option value={key.id}
        >{key.name}</option
      >{/each}
    {#if keyId && !projectKeysList.some((key) => key.id === keyId)}
      <option value={keyId}>{keyId} · current selection</option>
    {/if}</select
  >
  <label for="code-daily">Daily hard token limit (UTC)</label><input
    id="code-daily"
    inputmode="numeric"
    bind:value={daily}
    placeholder="No daily cap"
  />
  <label for="code-monthly">Monthly hard token limit (UTC)</label><input
    id="code-monthly"
    inputmode="numeric"
    bind:value={monthly}
    placeholder="No monthly cap"
  />
  <p>
    Hard budgets are optional. Every matching budget reserves a proven safe
    bound before each generation. Operations without a proven bound are refused
    only when a matching hard budget is enabled. No output limit is injected.
  </p>
  <p>
    Earlier usage and unknown consumption count. Disabling a budget does not
    erase reservations. Scope is immutable; all matching route, key and project
    budgets apply together.
  </p>
  <label class="choice"
    ><input type="checkbox" bind:checked={enabled} />Enabled</label
  >
</ResourceEditorForm>
