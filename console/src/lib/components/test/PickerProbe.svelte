<script lang="ts">
  import { personOptions } from '../../features/identity/users/personOptions.js';
  import EntityPicker from '../EntityPicker.svelte';

  let { picked }: { picked: (value: string) => void } = $props();
</script>

<EntityPicker
  id="probe"
  source={personOptions}
  emptyLabel="Anyone"
  onchange={picked}
/>
