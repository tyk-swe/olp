<script lang="ts">
  import { SvelteURLSearchParams } from 'svelte/reactivity';
  import ProviderWizardRun from './ProviderWizardRun.svelte';

  const copy = new SvelteURLSearchParams(globalThis.location?.search).get(
    'copy'
  );
  let restarts = $state(0);
</script>

{#key restarts}
  <ProviderWizardRun
    copyFrom={restarts ? null : copy}
    onAddAnother={() => (restarts += 1)}
  />
{/key}
