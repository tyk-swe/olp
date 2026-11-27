<script lang="ts">
  import NavIcon from '#lib/components/NavIcon.svelte';
  import { toasts } from '#lib/components/toasts.svelte.js';
</script>

<div class="toaster" aria-live="polite" role="status">
  {#each toasts.items as toast (toast.id)}
    <div
      class="toast {toast.tone}"
      role="group"
      onpointerenter={() => toasts.pause(toast.id)}
      onpointerleave={() => toasts.resume(toast.id)}
      onfocusin={() => toasts.pause(toast.id)}
      onfocusout={() => toasts.resume(toast.id)}
    >
      <p>{toast.message}</p>
      <button
        type="button"
        aria-label="Dismiss"
        onclick={() => toasts.dismiss(toast.id)}
      >
        <NavIcon name="close" size={16} />
      </button>
    </div>
  {/each}
</div>

<style>
  .toaster {
    position: fixed;
    z-index: 60;
    right: 1rem;
    bottom: 1rem;
    display: grid;
    width: min(calc(100% - 2rem), 24rem);
    gap: 0.5rem;
    pointer-events: none;
  }

  .toast {
    --tone: var(--success);
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.75rem 0.5rem 0.75rem 1rem;
    border: 1px solid var(--border);
    border-left: 2px solid var(--tone);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
    box-shadow: var(--shadow-overlay);
    font-size: var(--text-body-sm);
    pointer-events: auto;
    animation: pop-in 240ms var(--ease-out);
  }

  .toast.info {
    --tone: var(--signal);
  }

  .toast.warning {
    --tone: var(--warning);
  }

  .toast.danger {
    --tone: var(--danger);
  }

  p {
    flex: 1;
  }

  button {
    display: grid;
    place-items: center;
    width: 2rem;
    height: 2rem;
    flex: none;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground-muted);
    transition:
      background-color var(--motion),
      color var(--motion);
  }

  button:hover {
    background: var(--surface-hover);
    color: var(--foreground);
  }
</style>
