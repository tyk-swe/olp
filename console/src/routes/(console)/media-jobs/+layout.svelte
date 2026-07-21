<script lang="ts">
  import { setContext } from 'svelte';
  import {
    mediaJobListStateContext,
    type MediaJobListState
  } from '$lib/features/operations/mediaJobListState';

  let { children } = $props();
  const listState = $state<MediaJobListState>({
    route: '',
    jobState: '',
    lifecycle: '',
    cursor: undefined,
    history: [],
    applied: { limit: 25 }
  });

  setContext(mediaJobListStateContext, listState);
</script>

{@render children()}
