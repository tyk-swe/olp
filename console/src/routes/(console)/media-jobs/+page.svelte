<script lang="ts">
  import MediaJobsPage from '$lib/features/operations/media-jobs/MediaJobsPage.svelte';
  import { getMediaJobListState } from '$lib/features/operations/media-jobs/mediaJobListState';

  let listState = getMediaJobListState();
</script>

<MediaJobsPage bind:listState />
