<script lang="ts">
  import { untrack } from 'svelte';
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import RouteDraftEditor from '../RouteDraftEditor.svelte';
  import RoutingPolicyEditor from '../RoutingPolicyEditor.svelte';

  type Scope = 'installation' | 'route-draft' | 'api-key';
  let {
    client,
    routeId = 'route-a',
    keyed = false,
    policy = false,
    policyScope = 'route-draft' as Scope,
    onPolicySaved = () => {}
  }: {
    client: QueryClient;
    routeId?: string;
    keyed?: boolean;
    policy?: boolean;
    policyScope?: Scope;
    onPolicySaved?: (
      etag: string,
      previousEtag: string
    ) => void | Promise<void>;
  } = $props();
  let currentId = $state(untrack(() => routeId));
  let currentScope = $state(untrack(() => policyScope));
  let policyDirty = $state(false);
  let policyBusy = $state(false);
  let mounted = $state(true);
</script>

<QueryClientProvider {client}>
  <div class="probe-controls">
    {#each ['route-a', 'route-b', 'route-c'] as id (id)}
      <button type="button" onclick={() => (currentId = id)}>Open {id}</button>
    {/each}
    {#if policy}
      {#each ['installation', 'route-draft', 'api-key'] as scope (scope)}
        <button type="button" onclick={() => (currentScope = scope as Scope)}
          >Scope {scope}</button
        >
      {/each}
      <p data-probe="policy-flags">
        dirty:{policyDirty} busy:{policyBusy}
      </p>
    {/if}
    <button type="button" onclick={() => (mounted = !mounted)}>
      {mounted ? 'Unmount editor' : 'Remount editor'}
    </button>
  </div>
  {#if mounted}
    {#if policy}
      <RoutingPolicyEditor
        scope={currentScope}
        id={currentId}
        resourceEtag=""
        canManage={true}
        bind:dirty={policyDirty}
        bind:busy={policyBusy}
        onSaved={onPolicySaved}
      />
    {:else if keyed}
      {#key currentId}
        <RouteDraftEditor routeId={currentId} />
      {/key}
    {:else}
      <RouteDraftEditor routeId={currentId} />
    {/if}
  {/if}
</QueryClientProvider>
