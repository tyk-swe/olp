<script lang="ts">
  import { onMount } from 'svelte';
  import { readResult } from '#lib/editing/result.js';
  import { errorMessage } from '#lib/management/http.js';
  import { getSetupStatus } from '#lib/features/access/setup/api.js';
  import OwnerSetup from '#lib/features/access/setup/OwnerSetup.svelte';
  import SetupFrame from '#lib/shell/SetupFrame.svelte';
  import BrandMark from '#lib/shell/BrandMark.svelte';

  let { onDone }: { onDone: () => void } = $props();

  let setupView = $state<'checking' | 'ready' | 'no-token' | 'error'>(
    'checking'
  );
  let message = $state('');
  let activeController: AbortController | undefined;

  async function checkStatus() {
    activeController?.abort();
    const controller = new AbortController();
    activeController = controller;
    setupView = 'checking';
    message = '';
    const result = await readResult(() => getSetupStatus(controller.signal));
    if (controller.signal.aborted) return;
    if (!result.ok) {
      message = errorMessage(
        result.error,
        'The setup status could not be loaded.'
      );
      setupView = 'error';
      return;
    }
    if (!result.value.setup_required) {
      onDone();
      return;
    }
    setupView = result.value.bootstrap_token_configured ? 'ready' : 'no-token';
  }

  onMount(() => {
    void checkStatus();
    return () => activeController?.abort();
  });
</script>

<SetupFrame>
  {#if setupView === 'checking'}
    <div class="status-card" role="status">
      <BrandMark size={40} animated />
      <h2>Checking this installation</h2>
      <p>
        The console is asking the local control API whether first-run setup is
        required.
      </p>
    </div>
  {:else if setupView === 'error'}
    <div class="status-card" role="alert">
      <span class="error-symbol" aria-hidden="true">!</span>
      <h2>Setup is not reachable</h2>
      <p>{message}</p>
      <button class="button button-primary" type="button" onclick={checkStatus}
        >Try again</button
      >
    </div>
  {:else if setupView === 'no-token'}
    <div class="status-card" role="alert">
      <span class="error-symbol" aria-hidden="true">!</span>
      <h2>No setup token on the server</h2>
      <p>
        The owner account can only be created with the server's one-time setup
        token, and this server has none it can read. Mount one at
        <code>OLP_BOOTSTRAP_TOKEN_FILE</code>, or restart with Docker Compose,
        which generates it on first start.
      </p>
      <button class="button button-primary" type="button" onclick={checkStatus}
        >Check again</button
      >
    </div>
  {:else}
    <OwnerSetup onComplete={onDone} />
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
    font-weight: 400;
    letter-spacing: -0.031em;
    line-height: 1.1;
  }

  .status-card p {
    margin: 0.65rem 0 1.25rem;
    color: var(--foreground-muted);
  }

  .error-symbol {
    display: grid;
    width: 2.5rem;
    height: 2.5rem;
    place-items: center;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    color: var(--danger);
    font-size: 1.25rem;
    font-weight: 500;
  }
</style>
