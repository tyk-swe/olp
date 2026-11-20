<script lang="ts">
  import BrandMark from '#lib/shell/BrandMark.svelte';
  import { useBranding } from './branding';

  let { size }: { size: number } = $props();
  const branding = useBranding();
</script>

{#if branding.data?.logo}<img
    class="installation-logo"
    src={branding.data.logo}
    alt=""
    width={size}
    height={size}
  />{:else}<BrandMark {size} />{/if}

<style>
  .installation-logo {
    object-fit: contain;
    border-radius: var(--radius-control);
  }
</style>
