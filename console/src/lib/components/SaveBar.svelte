<script lang="ts">
  let {
    form,
    dirty,
    busy = false,
    saveLabel = 'Save changes',
    disabled = false,
    onDiscard
  }: {
    form: string;
    dirty: boolean;
    busy?: boolean;
    saveLabel?: string;
    disabled?: boolean;
    onDiscard?: () => void;
  } = $props();
</script>

<div class="save-bar" class:dirty>
  <p class="state" aria-live="polite">
    {busy ? 'Saving…' : dirty ? 'Unsaved changes' : 'All changes saved'}
  </p>
  <div class="actions">
    {#if onDiscard && dirty}
      <button
        class="button button-secondary"
        type="button"
        disabled={busy}
        onclick={onDiscard}>Discard</button
      >
    {/if}
    <button
      class="button button-primary"
      type="submit"
      {form}
      disabled={disabled || busy || !dirty}
      aria-busy={busy}>{busy ? 'Saving…' : saveLabel}</button
    >
  </div>
</div>

<style>
  .save-bar {
    position: sticky;
    z-index: 5;
    bottom: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    margin-top: 1.5rem;
    padding: 0.75rem 1rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-card) var(--radius-card) 0 0;
    background: color-mix(in srgb, var(--surface) 88%, transparent);
    backdrop-filter: blur(12px);
    -webkit-backdrop-filter: blur(12px);
  }

  .save-bar.dirty {
    border-color: color-mix(in srgb, var(--signal) 45%, var(--border));
  }

  .state {
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    text-transform: uppercase;
  }

  .dirty .state {
    color: var(--accent-strong);
  }

  .actions {
    display: flex;
    gap: 0.5rem;
  }
</style>
