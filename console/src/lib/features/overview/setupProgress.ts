import {
  can,
  type Capability,
  type FixedRole
} from '$lib/features/access/session/authorization';

type SetupInput = {
  loading: boolean;
  failed: boolean;
  activeProvider: boolean;
  enabledModels: boolean;
  activeRoute: boolean;
  apiKey: boolean;
  role: FixedRole | null;
};

const definitions = [
  {
    label: 'Create the installation owner',
    description: 'Local authentication is ready.',
    href: '/settings/profile',
    viewHref: '/settings/profile'
  },
  {
    label: 'Connect and activate a provider',
    description: 'Connect an upstream, test it, and activate the provider.',
    href: '/providers/new',
    viewHref: '/providers',
    capability: 'providers.manage'
  },
  {
    label: 'Review discovered models',
    description: 'Enable the certified model capabilities your clients need.',
    href: '/models',
    viewHref: '/models',
    capability: 'providers.manage'
  },
  {
    label: 'Build and activate a route',
    description:
      'Choose your model targets, simulate the route, then activate it.',
    href: '/routes/new',
    viewHref: '/routes',
    capability: 'routes.manage'
  },
  {
    label: 'Create your first API key',
    description: 'Create a scoped key. Its secret is shown only once.',
    href: '/api-keys/new',
    viewHref: '/api-keys',
    capability: 'api_keys.manage'
  }
] as const satisfies readonly {
  label: string;
  description: string;
  href: string;
  viewHref: string;
  capability?: Capability;
}[];

export function setupProgress(input: SetupInput) {
  const settled = !input.loading && !input.failed;
  const completed = settled
    ? [
        true,
        input.activeProvider,
        input.enabledModels,
        input.activeRoute,
        input.apiKey
      ]
    : [];
  const currentIndex = completed.findIndex((value) => !value);
  const steps = definitions.map((definition, index) => {
    const allowed =
      !('capability' in definition) || can(input.role, definition.capability);
    return {
      label: definition.label,
      description: definition.description,
      href:
        allowed && !completed[index] ? definition.href : definition.viewHref,
      allowed,
      complete: completed[index] ?? false,
      current: index === currentIndex,
      permissionNote:
        allowed || completed[index]
          ? ''
          : 'Ask an owner or operator to complete this step. You can view the current configuration.'
    };
  });
  const completeCount = completed.filter(Boolean).length;
  return {
    loading: input.loading,
    failed: input.failed,
    settled,
    completeCount,
    complete: settled && completeCount === definitions.length,
    steps,
    nextStep: settled
      ? steps.find((step) => !step.complete && step.allowed)
      : undefined
  };
}

export type SetupProgress = ReturnType<typeof setupProgress>;
