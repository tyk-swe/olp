<script lang="ts">
  import type { CodeAccount, CodePool, CodeRoute } from '$lib/api/code-mode';
  import type { PaidTargetOption, CodeEditing } from './presentation';
  import AccountEditor from './AccountEditor.svelte';
  import PoolEditor from './PoolEditor.svelte';
  import RouteEditor from './RouteEditor.svelte';
  import BudgetEditor from './BudgetEditor.svelte';

  let {
    editing,
    projectId,
    accounts,
    pools,
    providers,
    routes,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: {
    editing: CodeEditing;
    projectId: string;
    accounts: CodeAccount[];
    pools: CodePool[];
    providers: PaidTargetOption[];
    routes: CodeRoute[];
    allowed: boolean;
    onDraftChange: (dirty: boolean) => void;
    onSaved: () => Promise<void>;
    onCancel: () => void;
  } = $props();

  const shared = $derived({
    projectId,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  });
</script>

{#if editing.kind === 'accounts'}
  <AccountEditor current={editing.current} {...shared} />
{:else if editing.kind === 'pools'}
  <PoolEditor current={editing.current} {accounts} {...shared} />
{:else if editing.kind === 'routes'}
  <RouteEditor
    current={editing.current}
    {pools}
    {accounts}
    {providers}
    {...shared}
  />
{:else}
  <BudgetEditor current={editing.current} {routes} {...shared} />
{/if}
