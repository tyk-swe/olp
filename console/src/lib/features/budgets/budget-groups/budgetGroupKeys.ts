const root = ['budget-groups'] as const;

export const budgetGroupKeys = {
  root,
  list: () => [...root, 'list'] as const
};
