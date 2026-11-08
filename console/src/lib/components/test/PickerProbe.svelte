<script lang="ts">
  import EntityPicker from '../EntityPicker.svelte';

  let { picked }: { picked: (value: string) => void } = $props();
</script>

<EntityPicker
  id="probe"
  kind="person"
  emptyLabel="Anyone"
  onchange={picked}
/>
