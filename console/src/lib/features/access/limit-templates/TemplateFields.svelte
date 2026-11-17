<script lang="ts">
  import NamedLimitFields from '../budgets/NamedLimitFields.svelte';
  import type { NamedLimitRow } from '../budgets/namedLimits';

  let {
    form = $bindable(),
    disabled = false
  }: { form: NamedLimitRow[]; disabled?: boolean } = $props();
</script>

<NamedLimitFields kind="template" bind:form {disabled} />
