import type { ActiveRoute } from './api.js';

export function selectActiveRoutes(routes: ActiveRoute[]): ActiveRoute[] {
  return routes.filter((route) => route.state === 'active');
}
