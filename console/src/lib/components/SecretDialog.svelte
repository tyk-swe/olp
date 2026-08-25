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

  let dialog: HTMLDialogElement;

  onMount(() => {
    const opener = document.activeElement;
    document.body.classList.add('secret-dialog-open');
    dialog.showModal();
    dialog.querySelector<HTMLElement>('[data-autofocus]')?.focus();
    return () => {
      document.body.classList.remove('secret-dialog-open');
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  function close() {
    dialog.close();
  }

  function preventDismiss(event: Event) {
    event.preventDefault();
  }
</script>

<dialog
  class="secret-dialog card"
  class:wide={size === 'wide'}
  bind:this={dialog}
  aria-labelledby="secret-dialog-title"
  aria-describedby="secret-dialog-description"
  oncancel={preventDismiss}
  onclose={() => onClose()}
>
  <p class="eyebrow">{eyebrow}</p>
  <h2 id="secret-dialog-title" class="dialog-title">{title}</h2>
  <p id="secret-dialog-description" class="dialog-description">{description}</p>
  {@render children(close)}
</dialog>

<style>
  :global(body.secret-dialog-open) { overflow: hidden; }

  .secret-dialog::backdrop {
    background: rgb(11 17 30 / 62%);
    backdrop-filter: blur(4px);
  }

  .secret-dialog {
    position: fixed;
    top: 2rem;
    width: min(calc(100% - 2rem), 38rem);
    max-height: calc(100dvh - 4rem);
    margin: 0 auto;
    overflow-y: auto;
    padding: clamp(1.25rem, 4vw, 2rem);
    color: var(--foreground);
    box-shadow: var(--shadow-md);
  }

  .secret-dialog.wide { width: min(calc(100% - 2rem), 56rem); }

  .dialog-title {
    margin: 0;
    font-size: clamp(1.5rem, 4vw, 2rem);
    font-weight: 730;
    letter-spacing: -.035em;
    line-height: 1.15;
  }

  .dialog-description { margin: .65rem 0 1rem; color: var(--foreground-muted); }

  @media (max-width: 38rem) {
    .secret-dialog { top: .75rem; max-height: calc(100dvh - 1.5rem); }
  }

  @media (forced-colors: active) {
    .secret-dialog::backdrop { background: Canvas; opacity: .85; }
  }
</style>
