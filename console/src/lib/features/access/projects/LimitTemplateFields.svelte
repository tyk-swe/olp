<script lang="ts">
  import {
    NamedLimitFields,
    type NamedLimitRow
  } from '#lib/features/budgets/index.js';

  let {
    form = $bindable(),
    disabled = false
  }: { form: NamedLimitRow[]; disabled?: boolean } = $props();
</script>

<NamedLimitFields kind="template" bind:form {disabled} />
