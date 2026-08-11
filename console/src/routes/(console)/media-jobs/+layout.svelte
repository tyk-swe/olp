<script lang="ts">
  import {
    emptyMediaJobListState,
    setMediaJobListState,
    type MediaJobListState
  } from '$lib/features/operations/media-jobs/mediaJobListState';

  let { children } = $props();
  const listState = $state<MediaJobListState>(emptyMediaJobListState());

  setMediaJobListState(listState);
</script>

{@render children()}
