<script lang="ts">
  import { productName, useBranding } from './branding';

  let { title }: { title: string } = $props();
  const branding = useBranding();
</script>

<svelte:head>
  <title>{title} · {branding.data?.name ?? productName}</title>
</svelte:head>
