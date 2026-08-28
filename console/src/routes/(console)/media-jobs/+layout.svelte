<script lang="ts">
  import { mediaJobList } from '$lib/features/operations/media-jobs/mediaJobListState';

  let { children } = $props();
  // The list keeps its cursor (and filters) across this route family, so a
  // detail visit returns to the same page; leaving the family resets it.
  const listState = $state(mediaJobList.empty());
  mediaJobList.set(listState);
</script>

{@render children()}
