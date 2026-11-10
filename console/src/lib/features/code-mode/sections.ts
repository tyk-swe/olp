const managementKinds = ['accounts', 'pools', 'routes', 'budgets'] as const;
export type CodeManagementKind = (typeof managementKinds)[number];

const diagnosticKinds = [
  'bindings',
  'attempts',
  'refusals',
  'token-windows'
] as const;
export type CodeDiagnosticsKind = (typeof diagnosticKinds)[number];

export type CodeSection = CodeManagementKind | CodeDiagnosticsKind;

export const managementSections: Record<
  CodeManagementKind,
  { label: string; title: string; noun: string; empty: string }
> = {
  accounts: {
    label: 'Accounts',
    title: 'Subscription accounts',
    noun: 'account',
    empty: 'accounts'
  },
  pools: {
    label: 'Pools',
    title: 'Account pools',
    noun: 'pool',
    empty: 'pools'
  },
  routes: {
    label: 'Routes',
    title: 'Native-model routes',
    noun: 'route',
    empty: 'routes'
  },
  budgets: {
    label: 'Token budgets',
    title: 'Optional hard token budgets',
    noun: 'budget',
    empty: 'budgets'
  }
};

export const diagnosticSections: Record<
  CodeDiagnosticsKind,
  { label: string; title: string; empty: string }
> = {
  bindings: {
    label: 'Conversation trees',
    title: 'Durable conversation trees',
    empty: 'bindings'
  },
  attempts: {
    label: 'Attempts',
    title: 'Generation attempts',
    empty: 'attempts'
  },
  refusals: {
    label: 'Refusals',
    title: 'Admission refusals',
    empty: 'refusals'
  },
  'token-windows': {
    label: 'Token windows',
    title: 'Hard-budget token windows',
    empty: 'token windows'
  }
};

export const codeSections: readonly { id: CodeSection; label: string }[] = [
  ...managementKinds.map((id) => ({
    id,
    label: managementSections[id].label
  })),
  ...diagnosticKinds.map((id) => ({
    id,
    label: diagnosticSections[id].label
  }))
];
