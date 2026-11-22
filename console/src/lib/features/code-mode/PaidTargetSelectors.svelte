<script lang="ts">
  import type { CodeRouteWrite } from '#lib/features/code-mode/api.js';
  import type { PaidTargetOption } from './presentation';

  type PaidTargets = NonNullable<CodeRouteWrite['paid_targets']>;

  let {
    targets,
    prefix,
    label,
    emptyLabel,
    onchange,
    models,
    providers
  }: {
    targets: PaidTargets;
    prefix: string;
    label: string;
    emptyLabel: string;
    onchange: (targets: PaidTargets) => void;
    models: string[];
    providers: PaidTargetOption[];
  } = $props();
</script>

{#each models as model (model)}
  {@const target = targets[model]}
  <div class="form-field">
    <label for={prefix + '-provider-' + model}
      >{label} provider for {model}</label
    >
    <select
      id={prefix + '-provider-' + model}
      value={target?.provider_id ?? ''}
      onchange={(event) =>
        onchange({
          ...targets,
          [model]: { provider_id: event.currentTarget.value, slot_id: '' }
        })}
    >
      <option value="">{emptyLabel}</option>
      {#each providers as provider (provider.id)}<option value={provider.id}
          >{provider.name}</option
        >{/each}
      {#if target?.provider_id && !providers.some((provider) => provider.id === target.provider_id)}<option
          value={target.provider_id}
          >{target.provider_id} · current selection</option
        >{/if}
    </select>
    {#if target?.provider_id}
      <label for={prefix + '-slot-' + model}
        >{label} credential slot for {model}</label
      >
      <select
        id={prefix + '-slot-' + model}
        value={target.slot_id ?? ''}
        onchange={(event) =>
          onchange({
            ...targets,
            [model]: {
              provider_id: target.provider_id,
              slot_id: event.currentTarget.value
            }
          })}
        required
      >
        <option value="">Choose a credential slot</option>
        {#each providers.find((provider) => provider.id === target.provider_id)?.slots ?? [] as slot (slot.id)}<option
            value={slot.id}>{slot.name || slot.id}</option
          >{/each}
      </select>
    {/if}
  </div>
{/each}
