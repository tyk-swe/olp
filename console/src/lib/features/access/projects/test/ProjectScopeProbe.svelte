<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProjectScopeField from '../ProjectScopeField.svelte';

  let { client, unassigned }: { client: QueryClient; unassigned?: boolean } =
    $props();
  let value = $state('');
</script>

<QueryClientProvider {client}
  ><ProjectScopeField bind:value {unassigned} /></QueryClientProvider
>
