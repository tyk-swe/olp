<script lang="ts">
  import { page } from '$app/state';
  import TaskDetailPage from '#lib/features/agents/TaskDetailPage.svelte';
</script>

{#key page.params.taskId}
  <TaskDetailPage taskID={page.params.taskId ?? ''} />
{/key}
