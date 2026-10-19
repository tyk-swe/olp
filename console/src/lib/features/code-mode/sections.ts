/** The management sections, in navigation order. They edit configuration;
 * every other section only observes traffic. */
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

type DiagnosticFilters = {
  route: boolean;
  key: boolean;
  account: boolean;
  binding: boolean;
  endUser: boolean;
};

/** Route and key filters apply to every diagnostic, except token windows,
 * which have no filter form. */
function filters(
  overrides: Partial<DiagnosticFilters> = {}
): DiagnosticFilters {
  return {
    route: true,
    key: true,
    account: false,
    binding: false,
    endUser: false,
    ...overrides
  };
}

export const diagnosticSections: Record<
  CodeDiagnosticsKind,
  { label: string; title: string; empty: string; filters: DiagnosticFilters }
> = {
  bindings: {
    label: 'Conversation trees',
    title: 'Durable conversation trees',
    empty: 'bindings',
    filters: filters({ account: true })
  },
  attempts: {
    label: 'Attempts',
    title: 'Generation attempts',
    empty: 'attempts',
    filters: filters({ account: true, binding: true, endUser: true })
  },
  refusals: {
    label: 'Refusals',
    title: 'Admission refusals',
    empty: 'refusals',
    filters: filters({ endUser: true })
  },
  'token-windows': {
    label: 'Token windows',
    title: 'Hard-budget token windows',
    empty: 'token windows',
    filters: filters({ route: false, key: false })
  }
};

/** Tab labels in page order. */
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

const management = new Set<CodeSection>(managementKinds);

export function isManagementSection(
  section: CodeSection
): section is CodeManagementKind {
  return management.has(section);
}
