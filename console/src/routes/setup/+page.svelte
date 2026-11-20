<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { brandingKey } from '#lib/shell/branding/branding.js';
  import SetupPage from '#lib/features/access/setup/SetupPage.svelte';

  const queryClient = useQueryClient();

  function finishSetup() {
    void queryClient.invalidateQueries({ queryKey: brandingKey });
    void goto(resolve(''), { replace: true });
  }
</script>

<svelte:head>
  <title>Set up OpenLLMProxy</title>
  <meta
    name="description"
    content="Create the owner account for this OpenLLMProxy installation."
  />
</svelte:head>

<SetupPage onDone={finishSetup} />
