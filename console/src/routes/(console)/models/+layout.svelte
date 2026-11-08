<script lang="ts">
  import SectionLayout from '#lib/shell/SectionLayout.svelte';

  let { children } = $props();
</script>

<SectionLayout>{@render children()}</SectionLayout>
