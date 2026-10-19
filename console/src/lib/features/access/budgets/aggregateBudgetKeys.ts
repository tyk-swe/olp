import { projectKeys } from '../projects/projectKeys';

const root = [...projectKeys.root, 'aggregate-budget'] as const;

export const aggregateBudgetKeys = {
  root,
  detail: (scopeId: string) => [...root, scopeId] as const
};
