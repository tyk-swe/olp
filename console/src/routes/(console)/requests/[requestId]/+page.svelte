<script lang="ts">
  import { page } from '$app/state';
  import RequestsPage from '$lib/features/operations/requests/RequestsPage.svelte';
  import { getRequestListState } from '$lib/features/operations/requests/requestListState';

  let listState = getRequestListState();
</script>

<RequestsPage requestId={page.params.requestId} bind:listState />
