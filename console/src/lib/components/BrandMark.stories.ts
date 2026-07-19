import type { Meta, StoryObj } from '@storybook/svelte';
import BrandMark from './BrandMark.svelte';

const meta = {
  title: 'Foundation/Routing mark',
  component: BrandMark,
  args: {
    decorative: false,
    label: 'OpenLLMProxy routing mark',
    size: 64
  },
  parameters: {
    docs: {
      description: {
        component: 'A code-native routing mark: two inputs resolve through an ordered route into two targets.'
      }
    }
  }
} satisfies Meta<typeof BrandMark>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {};
