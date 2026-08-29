<script lang="ts">
  import MediaJobsPage from '$lib/features/operations/media-jobs/MediaJobsPage.svelte';
  import { mediaJobList } from '$lib/features/operations/media-jobs/mediaJobListState';

  let listState = mediaJobList.get();
</script>

<MediaJobsPage bind:listState />
