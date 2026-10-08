import type { components } from '$lib/api/schema';
export type RouteGroups = components['schemas']['RouteGroups'];
export type GroupForm = { id: string; name: string; routes: string }[];
export const groupNames = (value: string) =>
  value.split(/[\s,]+/).filter(Boolean);
const slug = /^[a-z0-9][a-z0-9._-]{0,99}$/;
export function groupNamesError(names: string[]): string {
  return names.length > 64 ||
    new Set(names).size !== names.length ||
    names.some((name) => !slug.test(name))
    ? 'Use at most 64 unique group names with lowercase letters, numbers, dots, underscores or hyphens.'
    : '';
}
export function groupForm(groups: RouteGroups = {}): GroupForm {
  return Object.entries(groups).map(([name, routes]) => ({
    id: crypto.randomUUID(),
    name,
    routes: routes.join(', ')
  }));
}
export function groupError(form: GroupForm): string {
  const namesError = groupNamesError(form.map((row) => row.name));
  if (namesError) return namesError;
  for (const row of form) {
    const routes = groupNames(row.routes);
    if (
      routes.length > 100 ||
      new Set(routes).size !== routes.length ||
      routes.some((route) => !slug.test(route))
    )
      return 'Each group accepts at most 100 unique valid route slugs.';
  }
  return '';
}
export function groupInput(form: GroupForm): RouteGroups {
  return Object.fromEntries(
    form.map((row) => [row.name, groupNames(row.routes)])
  );
}
