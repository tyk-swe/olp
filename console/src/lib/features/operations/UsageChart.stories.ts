import type { Meta, StoryObj } from '@storybook/svelte';
import UsageChart from './UsageChart.svelte';

const meta = {
  title: 'Operations/Accessible usage chart',
  component: UsageChart,
  parameters: {
    layout: 'padded',
    a11y: { test: 'error' }
  },
  args: {
    points: [
      { bucket: '2026-07-12T10:00:00Z', request_count: 18, input_tokens: '1500', output_tokens: '420', cached_input_tokens: '180', media_units: '0', estimated_cost: '0.17', currency: 'USD', unpriced_count: 0, incomplete_count: 0 },
      { bucket: '2026-07-12T11:00:00Z', request_count: 31, input_tokens: '2800', output_tokens: '910', cached_input_tokens: '240', media_units: '0', estimated_cost: '0.31', currency: 'USD', unpriced_count: 0, incomplete_count: 0 },
      { bucket: '2026-07-12T12:00:00Z', request_count: 24, input_tokens: '2100', output_tokens: '640', cached_input_tokens: '0', media_units: '0', estimated_cost: null, currency: 'USD', unpriced_count: 2, incomplete_count: 0 }
    ]
  }
} satisfies Meta<typeof UsageChart>;

export default meta;
type Story = StoryObj<typeof meta>;

export const CompleteAndUnpriced: Story = {};

export const Empty: Story = { args: { points: [] } };
