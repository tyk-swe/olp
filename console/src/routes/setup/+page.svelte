<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { onMount } from 'svelte';
  import { getSetupStatus } from '$lib/api/setup';
  import OwnerSetup from '$lib/features/setup/OwnerSetup.svelte';
  import SetupFrame from '$lib/features/setup/SetupFrame.svelte';

  let setupView = $state<'checking' | 'ready' | 'error'>('checking');
  let message = $state('');
  let activeController: AbortController | undefined;

  async function checkStatus() {
    activeController?.abort();
    activeController = new AbortController();
    setupView = 'checking';
    message = '';
    try {
      const status = await getSetupStatus(activeController.signal);
      if (!status.setup_required) {
        await goto(resolve('/'), { replaceState: true });
        return;
      }
      setupView = 'ready';
    } catch (error) {
      if (activeController.signal.aborted) return;
      message = error instanceof Error ? error.message : 'The setup status could not be loaded.';
      setupView = 'error';
    }
  }

  function finishSetup() {
    void goto(resolve('/'), { replaceState: true });
  }

  onMount(() => {
    void checkStatus();
    return () => activeController?.abort();
  });
</script>

<svelte:head>
  <title>Set up OpenLLMProxy</title>
  <meta name="description" content="Create the owner account for this OpenLLMProxy installation." />
</svelte:head>

<SetupFrame>
  {#if setupView === 'checking'}
    <div class="status-card" role="status">
      <span class="spinner" aria-hidden="true"></span>
      <h2>Checking this installation</h2>
      <p>The console is asking the local control API whether first-run setup is required.</p>
    </div>
  {:else if setupView === 'error'}
    <div class="status-card" role="alert">
      <span class="error-symbol" aria-hidden="true">!</span>
      <h2>Setup is not reachable</h2>
      <p>{message}</p>
      <button class="button button-primary" type="button" onclick={checkStatus}>Try again</button>
    </div>
  {:else}
    <OwnerSetup onComplete={finishSetup} />
  {/if}
</SetupFrame>

<style>
  .status-card {
    display: grid;
    justify-items: start;
  }

  .status-card h2 {
    margin: 1.25rem 0 0;
    font-size: 1.75rem;
    letter-spacing: -0.035em;
  }

  .status-card p {
    margin: 0.65rem 0 1.25rem;
    color: var(--foreground-muted);
  }

  .spinner,
  .error-symbol {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    place-items: center;
    border-radius: 0.5rem;
  }

  .spinner {
    border: 3px solid var(--border);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 700ms linear infinite;
  }

  .error-symbol {
    background: var(--danger-soft);
    color: var(--danger);
    font-size: 1.25rem;
    font-weight: 800;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }
</style>
