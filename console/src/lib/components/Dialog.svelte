<script lang="ts">
  import { onMount, type Snippet } from 'svelte';

  let {
    title,
    description,
    placement = 'center',
    size = 'normal',
    children,
    footer,
    canClose = () => true,
    onClose = () => {}
  }: {
    title: string;
    description?: string;
    placement?: 'center' | 'side';
    size?: 'normal' | 'wide';
    children: Snippet<[close: () => void]>;
    footer?: Snippet<[close: () => void]>;
    canClose?: () => boolean;
    onClose?: () => void;
  } = $props();

  const id = $props.id();
  let dialog: HTMLDialogElement;

  onMount(() => {
    const opener = document.activeElement;
    document.documentElement.classList.add('dialog-open');
    dialog.showModal();
    dialog.querySelector<HTMLElement>('[data-autofocus]')?.focus();
    return () => {
      if (!document.querySelector('dialog.dialog[open]'))
        document.documentElement.classList.remove('dialog-open');
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  const close = () => {
    if (canClose()) dialog.close();
  };
</script>

<dialog
  class="dialog {placement}"
  class:wide={size === 'wide'}
  bind:this={dialog}
  aria-labelledby="{id}-title"
  aria-describedby={description ? `${id}-description` : undefined}
  oncancel={(event) => {
    if (!canClose()) event.preventDefault();
  }}
  onclose={() => onClose()}
  onclick={(event) => {
    if (event.target === dialog) close();
  }}
>
  <div class="frame">
    <header>
      <h2 id="{id}-title">{title}</h2>
      <button class="close" type="button" aria-label="Close" onclick={close}
        >×</button
      >
    </header>
    {#if description}<p id="{id}-description" class="description">
        {description}
      </p>{/if}
    <div class="content">{@render children(close)}</div>
    {#if footer}<footer>{@render footer(close)}</footer>{/if}
  </div>
</dialog>

<style>
  :global(html.dialog-open) {
    overflow: hidden;
  }

  .dialog {
    max-width: none;
    max-height: none;
    padding: 0;
    border: 1px solid var(--border);
    background: var(--surface-raised);
    color: var(--foreground);
    box-shadow: var(--shadow-overlay);
  }

  .dialog::backdrop {
    background: var(--backdrop);
    backdrop-filter: blur(6px);
    -webkit-backdrop-filter: blur(6px);
  }

  .dialog[open]::backdrop {
    animation: fade 240ms var(--ease-out);
  }

  .center {
    width: min(calc(100% - 2rem), 34rem);
    max-height: calc(100dvh - 4rem);
    margin: 2rem auto auto;
    border-radius: var(--radius-panel);
  }

  .center.wide {
    width: min(calc(100% - 2rem), 52rem);
  }

  .center[open] {
    animation: pop-in 260ms var(--ease-out);
  }

  .side {
    width: min(100%, 36rem);
    height: 100dvh;
    margin: 0 0 0 auto;
    border-width: 0 0 0 1px;
    border-radius: 0;
  }

  .side.wide {
    width: min(100%, 48rem);
  }

  .side[open] {
    animation: slide-in 280ms var(--ease-out);
  }

  .frame {
    display: flex;
    max-height: inherit;
    height: 100%;
    flex-direction: column;
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 1.25rem 1.5rem 0;
  }

  h2 {
    font-size: 1.375rem;
    font-weight: 500;
    letter-spacing: -0.025em;
    line-height: 1.2;
  }

  .description {
    padding: 0.4rem 1.5rem 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }

  .content {
    min-height: 0;
    flex: 1 1 auto;
    overflow-y: auto;
    padding: 1.25rem 1.5rem 1.5rem;
  }

  footer {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.5rem;
    padding: 1rem 1.5rem;
    border-top: 1px solid var(--border-hairline);
    background: var(--surface);
  }

  .close {
    width: 2.5rem;
    height: 2.5rem;
    flex: none;
    margin-right: -0.5rem;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground-muted);
    font-size: 1.4rem;
    line-height: 1;
    transition:
      background-color var(--motion),
      color var(--motion);
  }

  .close:hover {
    background: var(--surface-hover);
    color: var(--foreground);
  }

  @keyframes slide-in {
    from {
      opacity: 0;
      transform: translateX(2rem);
    }
  }

  @media (max-width: 38rem) {
    .center {
      margin-top: 0.75rem;
      max-height: calc(100dvh - 1.5rem);
    }
  }

  @media (forced-colors: active) {
    .dialog::backdrop {
      background: Canvas;
      opacity: 0.85;
    }
  }
</style>
