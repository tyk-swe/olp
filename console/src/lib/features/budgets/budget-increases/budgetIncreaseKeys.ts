const root = ['budget-increases'] as const;
const requestsRoot = ['budget-increase-requests'] as const;

export const budgetIncreaseKeys = {
  root,
  page: (before?: string) => [...root, before] as const,
  requestsRoot,
  requestPage: (before?: string) => [...requestsRoot, before] as const
};
