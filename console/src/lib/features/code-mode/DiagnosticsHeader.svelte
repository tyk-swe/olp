<script lang="ts">
  let {
    title,
    fetching,
    onRefresh
  }: { title: string; fetching: boolean; onRefresh: () => void } = $props();
</script>

<div class="actions">
  <h2>{title}</h2>
  <button
    class="button button-secondary"
    type="button"
    onclick={onRefresh}
    disabled={fetching}>Refresh</button
  >
</div>
<p>
  Metadata only: no prompts, outputs, tool payloads, raw headers or credentials.
  Account recovery requires proven nonexecution and a safe continuation.
  Accepted work and uncertain consumption are never replayed
</p>

<style>
  .actions {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  h2 {
    flex: 1;
    font-size: 1.25rem;
    font-weight: 600;
  }
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
  }
</style>
