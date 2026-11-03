export function validRouteSlug(value: string): boolean {
  return /^[a-z0-9][a-z0-9._-]{0,99}$/.test(value);
}
