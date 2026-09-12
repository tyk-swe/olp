<script lang="ts">
  import { onMount } from 'svelte';

  let {
    title = 'Confirm your identity',
    description,
    busy = false,
    error = '',
    onConfirm,
    onCancel
  }: {
    title?: string;
    description: string;
    busy?: boolean;
    error?: string;
    onConfirm: (password: string) => void | Promise<void>;
    onCancel: () => void;
  } = $props();

  let dialog: HTMLDialogElement;
  let password = $state('');
  let localError = $state('');

  onMount(() => {
    const opener = document.activeElement;
    dialog.showModal();
    dialog.querySelector<HTMLElement>('[data-autofocus]')?.focus();
    return () => {
      password = '';
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!password) {
      localError = 'Enter your current password.';
      return;
    }
    localError = '';
    await onConfirm(password);
    password = '';
  }

  function cancel() {
    if (!busy) onCancel();
  }

  function handleCancel(event: Event) {
    event.preventDefault();
    cancel();
  }
</script>

<dialog
  class="reauth-dialog card card-light"
  bind:this={dialog}
  aria-labelledby="reauth-title"
  aria-describedby="reauth-description"
  aria-busy={busy}
  oncancel={handleCancel}
>
  <p class="eyebrow">Security</p>
  <h2 id="reauth-title">{title}</h2>
  <p id="reauth-description" class="dialog-description">{description}</p>
  <form onsubmit={submit} novalidate>
    <div class="form-field">
      <label for="reauth-password">Current password</label>
      <input
        id="reauth-password"
        type="password"
        autocomplete="current-password"
        bind:value={password}
        data-autofocus
        aria-invalid={localError ? 'true' : undefined}
        disabled={busy}
      />
    </div>
    {#if localError || error}<p class="inline-problem" role="alert">
        {localError || error}
      </p>{/if}
    <div class="dialog-actions">
      <button
        class="button button-secondary"
        type="button"
        onclick={cancel}
        disabled={busy}>Cancel</button
      >
      <button class="button button-primary" type="submit" disabled={busy}
        >{busy ? 'Verifying…' : 'Confirm'}</button
      >
    </div>
  </form>
</dialog>

<style>
  .reauth-dialog::backdrop {
    background: rgb(16 16 16 / 78%);
  }

  .reauth-dialog {
    position: fixed;
    top: 4rem;
    width: min(calc(100% - 2rem), 28rem);
    margin: 0 auto;
    padding: clamp(1.25rem, 4vw, 1.75rem);
    border-radius: var(--radius-panel);
    color: var(--foreground);
  }

  h2 {
    margin: 0;
    font-size: 1.5rem;
    font-weight: 400;
    letter-spacing: -0.02em;
    line-height: 1.1;
  }

  .dialog-description {
    margin: 0.5rem 0 1rem;
    color: var(--foreground-muted);
    font-size: 0.85rem;
  }
  .dialog-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.6rem;
    margin-top: 1.1rem;
  }

  @media (max-width: 38rem) {
    .reauth-dialog {
      top: 1rem;
    }
  }
</style>
