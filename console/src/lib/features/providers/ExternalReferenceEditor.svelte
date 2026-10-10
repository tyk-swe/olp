<script lang="ts">
  let {
    value = $bindable(''),
    disabled = false
  }: { value?: string; disabled?: boolean } = $props();
</script>

<label
  >Pinned external credential reference<textarea
    aria-label="Pinned external credential reference"
    class="filter-control"
    bind:value
    {disabled}
    rows="5"
    spellcheck="false"
    placeholder={'{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}'}
  ></textarea></label
>
<p class="muted">
  Use AWS Secrets Manager, GCP Secret Manager, Azure Key Vault or Vault KV v2
  with an immutable version. The server uses its workload identity and validates
  the resolved credential before activation.
</p>

<style>
  label {
    display: grid;
    gap: 0.5rem;
  }
  textarea {
    width: 100%;
    min-width: 0;
    font-family: var(--font-mono);
  }
  p {
    font-size: 0.875rem;
    line-height: 1.5;
  }
</style>
