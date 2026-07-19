<script lang="ts">
  import { Dialog } from 'bits-ui';
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

  let dialogOpen = $state(true);

  onMount(() => {
    document.body.classList.add('secret-dialog-open');
    return () => document.body.classList.remove('secret-dialog-open');
  });

  function close() {
    dialogOpen = false;
  }

  function handleOpenChangeComplete(value: boolean) {
    if (!value) onClose();
  }

  function preventDismiss(event: Event) {
    event.preventDefault();
  }
</script>

<Dialog.Root bind:open={dialogOpen} onOpenChangeComplete={handleOpenChangeComplete}>
  <Dialog.Portal>
    <Dialog.Overlay class="secret-overlay" />
    <Dialog.Content
      class={`secret-dialog card${size === 'wide' ? ' wide' : ''}`}
      trapFocus
      preventScroll={false}
      onEscapeKeydown={preventDismiss}
      onInteractOutside={preventDismiss}
    >
      <p class="eyebrow">{eyebrow}</p>
      <Dialog.Title level={2} class="dialog-title">{title}</Dialog.Title>
      <Dialog.Description class="dialog-description">{description}</Dialog.Description>
      {@render children(close)}
    </Dialog.Content>
  </Dialog.Portal>
</Dialog.Root>

<style>
  :global(body.secret-dialog-open) { overflow: hidden; }

  :global(.secret-overlay) {
    position: fixed;
    z-index: 80;
    inset: 0;
    background: rgb(11 17 30 / 62%);
    backdrop-filter: blur(4px);
  }

  :global(.secret-dialog) {
    position: fixed;
    z-index: 81;
    top: 2rem;
    left: 50%;
    width: min(calc(100% - 2rem), 38rem);
    max-height: calc(100dvh - 4rem);
    overflow-y: auto;
    padding: clamp(1.25rem, 4vw, 2rem);
    box-shadow: var(--shadow-md);
    transform: translateX(-50%);
  }

  :global(.secret-dialog.wide) { width: min(calc(100% - 2rem), 56rem); }

  :global(.dialog-title) {
    margin: 0;
    font-size: clamp(1.5rem, 4vw, 2rem);
    font-weight: 730;
    letter-spacing: -.035em;
    line-height: 1.15;
  }

  :global(.dialog-description) { margin: .65rem 0 1rem; color: var(--foreground-muted); }

  @media (max-width: 38rem) {
    :global(.secret-dialog) { top: .75rem; max-height: calc(100dvh - 1.5rem); }
  }

  @media (forced-colors: active) {
    :global(.secret-overlay) { background: Canvas; opacity: .85; }
  }
</style>
