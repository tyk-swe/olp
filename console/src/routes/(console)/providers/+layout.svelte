<script lang="ts">
  import { setContext } from 'svelte';
  import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';
  import { providerPaginationContext } from '$lib/features/gateway/providers/providerPagination';

  let { children } = $props();
  const pagination = $state<CursorHistory>(emptyCursorHistory());

  setContext(providerPaginationContext, pagination);
</script>

{@render children()}
