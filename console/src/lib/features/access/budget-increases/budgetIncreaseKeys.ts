const root = ['budget-increases'] as const;

export const budgetIncreaseKeys = {
  root,
  page: (before?: string) => [...root, before] as const
};
