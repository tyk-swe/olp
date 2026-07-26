<script lang="ts">
  import type { ConflictNoticeKind } from '$lib/forms/concurrentEdit';

  let {
    notice,
    onReload,
    disabled = false
  }: {
    notice: ConflictNoticeKind;
    onReload: () => void | Promise<void>;
    disabled?: boolean;
  } = $props();
</script>

{#if notice}
  <div class:conflict={notice === 'conflict'} class="concurrent-notice" role={notice === 'conflict' ? 'alert' : 'status'}>
    <div>
      <strong>{notice === 'conflict' ? 'This item changed elsewhere.' : 'A newer version is available.'}</strong>
      <span>{notice === 'conflict' ? 'Reload the latest version before saving again.' : 'Your unsaved changes have not been overwritten.'}</span>
      <small>Reloading discards your unsaved changes.</small>
    </div>
    <button class="button button-secondary" type="button" onclick={onReload} {disabled}>Reload</button>
  </div>
{/if}

<style>
  .concurrent-notice {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    margin: 1rem 0;
    padding: .85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--warning) 45%, var(--border));
    border-radius: .375rem;
    background: var(--warning-soft);
    color: var(--warning);
  }
  .concurrent-notice.conflict {
    border-color: color-mix(in srgb, var(--danger) 45%, var(--border));
    background: var(--danger-soft);
    color: var(--danger);
  }
  .concurrent-notice div { display: grid; gap: .15rem; }
  .concurrent-notice span, .concurrent-notice small {
    color: var(--foreground);
    font-size: .75rem;
  }
  @media (max-width: 36rem) { .concurrent-notice { align-items: stretch; flex-direction: column; } }
</style>
