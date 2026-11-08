<script lang="ts">
  import MediaJobsPage from '#lib/features/media/MediaJobsPage.svelte';
  import { mediaJobList } from '#lib/features/media/mediaJobListState.js';

  let listState = mediaJobList.get();
</script>

<MediaJobsPage bind:listState />
