<script lang="ts">
  import Dialog from '#lib/components/Dialog.svelte';

  let {
    title,
    body,
    confirmLabel,
    danger = false,
    onSettle
  }: {
    title: string;
    body: string;
    confirmLabel: string;
    danger?: boolean;
    onSettle: (confirmed: boolean) => void;
  } = $props();

  let confirmed = false;
</script>

<Dialog {title} description={body} onClose={() => onSettle(confirmed)}>
  {#snippet children(close)}
    <div class="choices">
      <button
        class="button button-secondary"
        type="button"
        data-autofocus
        onclick={close}>Cancel</button
      >
      <button
        class="button"
        class:button-primary={!danger}
        class:button-danger={danger}
        type="button"
        onclick={() => {
          confirmed = true;
          close();
        }}>{confirmLabel}</button
      >
    </div>
  {/snippet}
</Dialog>

<style>
  .choices {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.5rem;
  }
</style>
