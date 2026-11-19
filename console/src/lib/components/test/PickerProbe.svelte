<script lang="ts">
  import { personOptions } from '#lib/features/access/index.js';
  import EntityPicker from '../EntityPicker.svelte';

  let { picked }: { picked: (value: string) => void } = $props();
</script>

<EntityPicker
  id="probe"
  source={personOptions}
  emptyLabel="Anyone"
  onchange={picked}
/>
