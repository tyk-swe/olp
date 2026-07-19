import type { Meta, StoryObj } from '@storybook/svelte';
import OperatorPrimitivesStory from './OperatorPrimitivesStory.svelte';

const meta = {
  title: 'Foundation/Operator primitives',
  component: OperatorPrimitivesStory,
  parameters: { layout: 'fullscreen', a11y: { test: 'error' } }
} satisfies Meta<typeof OperatorPrimitivesStory>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Interactive: Story = {};
