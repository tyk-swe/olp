<script lang="ts">
  import NamedLimitFields from '../NamedLimitFields.svelte';
  import type { NamedLimitRow } from '../namedLimits';

  let {
    form = $bindable(),
    disabled = false
  }: { form: NamedLimitRow[]; disabled?: boolean } = $props();
</script>

<NamedLimitFields kind="template" bind:form {disabled} />
