<script lang="ts">
  import { page } from '$app/state';
  import ToolCallDetailPage from '#lib/features/agents/ToolCallDetailPage.svelte';
</script>

{#key page.params.id}<ToolCallDetailPage callID={page.params.id ?? ''} />{/key}
