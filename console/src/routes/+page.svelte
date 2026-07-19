<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { onMount } from 'svelte';
  import { getSetupStatus } from '$lib/api/setup';
  import AppShell from '$lib/components/AppShell.svelte';
  import Overview from '$lib/features/overview/Overview.svelte';

  let checking = $state(true);
  let connected = $state(false);
  let statusError = $state('');
  let activeController: AbortController | undefined;

  async function checkSetup() {
    activeController?.abort();
    activeController = new AbortController();
    checking = true;
    statusError = '';
    try {
      const status = await getSetupStatus(activeController.signal);
      connected = true;
      if (status.setup_required) {
        await goto(resolve('/setup'), { replaceState: true });
      }
    } catch (error) {
      if (activeController.signal.aborted) return;
      connected = false;
      statusError = error instanceof Error ? error.message : 'The setup status could not be loaded.';
    } finally {
      checking = false;
    }
  }

  onMount(() => {
    void checkSetup();
    return () => activeController?.abort();
  });
</script>

<svelte:head>
  <title>Overview · OpenLLMProxy</title>
  <meta name="description" content="Operate model providers, routes, keys, and usage from OpenLLMProxy." />
</svelte:head>

<AppShell>
  {#if statusError}
    <div class="problem-banner" role="alert">
      <div>
        <strong>Control API unavailable</strong>
        <p>{statusError} The console is showing its last neutral state.</p>
      </div>
      <button class="button button-secondary" type="button" onclick={checkSetup}>Retry</button>
    </div>
  {:else if checking}
    <p class="checking" role="status"><span aria-hidden="true"></span>Checking installation status…</p>
  {/if}
  <Overview controlConnected={connected} />
</AppShell>

<style>
  .checking {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.6rem;
    margin: 0 0 1rem;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }

  .checking span {
    width: 0.8rem;
    height: 0.8rem;
    border: 2px solid var(--border-strong);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 700ms linear infinite;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  @media (max-width: 36rem) {
    .problem-banner {
      display: grid;
    }
  }
</style>
