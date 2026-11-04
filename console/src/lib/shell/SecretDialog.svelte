<script lang="ts">
  import { onMount, type Snippet } from 'svelte';

  let {
    eyebrow,
    title,
    description,
    size = 'normal',
    children,
    onClose = () => {}
  }: {
    eyebrow: string;
    title: string;
    description: string;
    size?: 'normal' | 'wide';
    children: Snippet<[close: () => void]>;
    onClose?: () => void;
  } = $props();

  const dialogId = $props.id();
  let dialog: HTMLDialogElement;

  onMount(() => {
    const opener = document.activeElement;
    document.body.classList.add('secret-dialog-open');
    dialog.showModal();
    dialog.querySelector<HTMLElement>('[data-autofocus]')?.focus();
    return () => {
      if (
        !Array.from(
          document.querySelectorAll('dialog.secret-dialog[open]')
        ).some((other) => other !== dialog)
      )
        document.body.classList.remove('secret-dialog-open');
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  function close() {
    dialog.close();
  }
</script>

<dialog
  class="secret-dialog card"
  class:wide={size === 'wide'}
  bind:this={dialog}
  aria-labelledby={`${dialogId}-title`}
  aria-describedby={`${dialogId}-description`}
  onclose={() => onClose()}
>
  <div class="dialog-header">
    <p class="eyebrow">{eyebrow}</p>
    <button
      class="dialog-close"
      type="button"
      aria-label="Close"
      onclick={close}>×</button
    >
  </div>
  <h2 id={`${dialogId}-title`} class="dialog-title">{title}</h2>
  <p id={`${dialogId}-description`} class="dialog-description">{description}</p>
  {@render children(close)}
</dialog>

<style>
  :global(body.secret-dialog-open) {
    overflow: hidden;
  }

  .secret-dialog::backdrop {
    background: var(--backdrop);
    backdrop-filter: blur(8px);
    -webkit-backdrop-filter: blur(8px);
  }

  .secret-dialog[open]::backdrop {
    animation: fade 240ms var(--ease-out);
  }

  .secret-dialog {
    position: fixed;
    top: 2rem;
    width: min(calc(100% - 2rem), 38rem);
    max-height: calc(100dvh - 4rem);
    margin: 0 auto;
    overflow-y: auto;
    padding: clamp(1.25rem, 4vw, 2rem);
    border: 1px solid var(--border);
    border-radius: var(--radius-panel);
    background: var(--surface-raised);
    color: var(--foreground);
    box-shadow: var(--shadow-overlay);
  }

  .secret-dialog[open] {
    animation: pop-in 280ms var(--ease-out);
  }

  .secret-dialog.wide {
    width: min(calc(100% - 2rem), 56rem);
  }

  .dialog-title {
    margin: 0;
    font-size: clamp(1.75rem, 4vw, 2.25rem);
    font-weight: 500;
    letter-spacing: -0.031em;
    line-height: 1.1;
  }

  .dialog-description {
    margin: 0.65rem 0 1rem;
    color: var(--foreground-muted);
  }

  .dialog-header {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1rem;
  }

  .dialog-close {
    width: 2.75rem;
    height: 2.75rem;
    flex: none;
    margin: -0.5rem -0.5rem 0 0;
    border: 0;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground-muted);
    font-size: 1.4rem;
    line-height: 1;
    transition:
      background-color var(--motion),
      color var(--motion),
      transform var(--motion);
  }

  .dialog-close:hover {
    background: var(--surface-hover);
    color: var(--foreground);
    transform: rotate(90deg);
  }

  @media (max-width: 38rem) {
    .secret-dialog {
      top: 0.75rem;
      max-height: calc(100dvh - 1.5rem);
    }
  }

  @media (forced-colors: active) {
    .secret-dialog::backdrop {
      background: Canvas;
      opacity: 0.85;
    }
  }
</style>
