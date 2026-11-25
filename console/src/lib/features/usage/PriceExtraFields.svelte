<script lang="ts">
  import type {
    TierRow,
    ToolFeeRow,
    ContainerFeeRow,
    WindowPeriodRow
  } from './priceEntry.js';

  let {
    idPrefix,
    canEdit,
    rateFields,
    containerFees = $bindable(),
    toolFees = $bindable(),
    tiers = $bindable(),
    windowZone = $bindable(),
    windowPeriods = $bindable()
  }: {
    idPrefix: string;
    canEdit: boolean;
    rateFields: readonly (readonly [string, string])[];
    containerFees: ContainerFeeRow[];
    toolFees: ToolFeeRow[];
    tiers: TierRow[];
    windowZone: string;
    windowPeriods: WindowPeriodRow[];
  } = $props();

  const weekDays = [
    ['1', 'Mon'],
    ['2', 'Tue'],
    ['3', 'Wed'],
    ['4', 'Thu'],
    ['5', 'Fri'],
    ['6', 'Sat'],
    ['7', 'Sun']
  ] as const;
</script>

<fieldset class="extra-components" disabled={!canEdit}>
  <legend>Additional components</legend>
  <p class="muted">
    Tool fees bill each invocation; tiers replace the per-million rate set above
    an input threshold; windows multiply the charge by time of day.
  </p>
  <div class="component-rows" aria-label="Container session fees">
    <p>Container session fees by memory tier</p>
    {#each containerFees as row, index (index)}
      <div class="component-row">
        <label class="sr-only" for={`${idPrefix}-container-tier-${index}`}>
          Container memory tier
        </label>
        <input
          id={`${idPrefix}-container-tier-${index}`}
          bind:value={row.tier}
          placeholder="1g"
          class="mono"
        />
        <label class="sr-only" for={`${idPrefix}-container-fee-${index}`}>
          Fee per session
        </label>
        <input
          id={`${idPrefix}-container-fee-${index}`}
          bind:value={row.fee}
          inputmode="decimal"
          placeholder="0.03"
        />
        <button
          type="button"
          class="text-button"
          onclick={() => {
            containerFees = containerFees.filter(
              (_: ContainerFeeRow, i: number) => i !== index
            );
          }}
        >
          Remove
        </button>
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        containerFees = [...containerFees, { tier: '', fee: '' }];
      }}
    >
      Add container tier
    </button>
  </div>
  <div class="component-rows" aria-label="Tool fees">
    {#each toolFees as row, index (index)}
      <div class="component-row">
        <label class="sr-only" for={`${idPrefix}-tool-name-${index}`}>
          Tool name
        </label>
        <input
          id={`${idPrefix}-tool-name-${index}`}
          bind:value={row.name}
          placeholder="web_search"
          class="mono"
        />
        <label class="sr-only" for={`${idPrefix}-tool-fee-${index}`}>Fee</label>
        <input
          id={`${idPrefix}-tool-fee-${index}`}
          bind:value={row.fee}
          inputmode="decimal"
          placeholder="0.010"
        />
        <button
          type="button"
          class="text-button"
          onclick={() => {
            toolFees = toolFees.filter(
              (_: ToolFeeRow, i: number) => i !== index
            );
          }}
        >
          Remove
        </button>
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        toolFees = [...toolFees, { name: '', fee: '' }];
      }}
    >
      Add tool fee
    </button>
  </div>
  <div class="component-rows" aria-label="Input-token tiers">
    {#each tiers as row, index (index)}
      <div class="component-row tier-row">
        <label for={`${idPrefix}-tier-threshold-${index}`}>
          Above input tokens
        </label>
        <input
          id={`${idPrefix}-tier-threshold-${index}`}
          bind:value={row.threshold}
          inputmode="numeric"
          placeholder="1000000"
        />
        {#each rateFields.filter(([, value]) => value.trim() !== '') as [field, parentValue] (field)}
          <label class="sr-only" for={`${idPrefix}-tier-${index}-${field}`}>
            {field.replaceAll('_', ' ')}
          </label>
          <input
            id={`${idPrefix}-tier-${index}-${field}`}
            bind:value={row.rates[field]}
            inputmode="decimal"
            placeholder={parentValue}
            title={field.replaceAll('_', ' ')}
          />
        {/each}
        <button
          type="button"
          class="text-button"
          onclick={() => {
            tiers = tiers.filter((_: TierRow, i: number) => i !== index);
          }}
        >
          Remove
        </button>
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        const rates: Record<string, string> = {};
        for (const [field, value] of rateFields)
          if (value.trim() !== '') rates[field] = '';
        tiers = [...tiers, { threshold: '', rates }];
      }}
    >
      Add tier
    </button>
  </div>
  <div class="component-rows" aria-label="Time windows">
    <div class="component-row">
      <label for={`${idPrefix}-window-zone`}>Window time zone</label>
      <input
        id={`${idPrefix}-window-zone`}
        bind:value={windowZone}
        class="mono"
        placeholder="America/New_York"
      />
    </div>
    {#each windowPeriods as period, index (index)}
      <div class="component-row window-row">
        <div
          class="weekday-toggles"
          role="group"
          aria-label={`Window ${index + 1} days`}
        >
          {#each weekDays as [day, label] (day)}
            <button
              type="button"
              class:selected={period.days.includes(Number(day))}
              aria-pressed={period.days.includes(Number(day))}
              onclick={() => {
                const value = Number(day);
                period.days = period.days.includes(value)
                  ? period.days.filter((d: number) => d !== value)
                  : [...period.days, value];
              }}
            >
              {label}
            </button>
          {/each}
        </div>
        <label class="sr-only" for={`${idPrefix}-window-start-${index}`}>
          Start
        </label>
        <input
          id={`${idPrefix}-window-start-${index}`}
          bind:value={period.start}
          placeholder="00:00"
          inputmode="numeric"
        />
        <label class="sr-only" for={`${idPrefix}-window-end-${index}`}>
          End
        </label>
        <input
          id={`${idPrefix}-window-end-${index}`}
          bind:value={period.end}
          placeholder="24:00"
          inputmode="numeric"
        />
        <label class="sr-only" for={`${idPrefix}-window-multiplier-${index}`}>
          Multiplier
        </label>
        <input
          id={`${idPrefix}-window-multiplier-${index}`}
          bind:value={period.multiplier}
          inputmode="decimal"
          placeholder="0.75"
        />
        <button
          type="button"
          class="text-button"
          onclick={() => {
            windowPeriods = windowPeriods.filter(
              (_: WindowPeriodRow, i: number) => i !== index
            );
          }}
        >
          Remove
        </button>
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        windowPeriods = [
          ...windowPeriods,
          { days: [], start: '', end: '', multiplier: '' }
        ];
      }}
    >
      Add window period
    </button>
  </div>
</fieldset>
