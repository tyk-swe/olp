import type { components } from '#lib/management/schema.js';
import { isJsonObject } from '#lib/editing/json.js';

export type RouteBehavior = components['schemas']['RouteBehavior'];
export type RouteSelector = components['schemas']['RouteSelector'];
export type FallbackCondition = components['schemas']['FallbackCondition'];
export type RetryClass =
  'connect' | 'timeout' | 'rate_limit' | 'upstream_server';

export const fallbackConditions: [FallbackCondition, string][] = [
  ['exhausted', 'Every attempt failed'],
  ['context_window', 'Context window exceeded'],
  ['content_filter', 'Content filter refusal'],
  ['rate_limit', 'Rate limited'],
  ['budget', 'Spend cap exhausted']
];

export const retryClasses: [RetryClass, string][] = [
  ['connect', 'Connection failures'],
  ['timeout', 'Timeouts'],
  ['rate_limit', 'Rate limits'],
  ['upstream_server', 'Upstream server errors']
];

export function emptyBehavior(): RouteBehavior {
  return {
    fallbacks: [],
    selectors: [],
    retry: {},
    affinity: null,
    budget: null,
    passthrough: null,
    certification: 'certified',
    tool_execution: '',
    toolsets: [],
    responses_mcp_items: false
  };
}

export function parseSelectors(text: string): RouteSelector[] | string {
  if (!text.trim()) return [];
  const parsed = parseJson(text);
  return Array.isArray(parsed) && parsed.every(isSelector)
    ? parsed
    : 'Selectors must be a JSON array of selector objects.';
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

function isStringArray(value: unknown): value is string[] {
  return (
    Array.isArray(value) && value.every((item) => typeof item === 'string')
  );
}

function isSelector(value: unknown): value is RouteSelector {
  if (
    !isJsonObject(value) ||
    typeof value.id !== 'string' ||
    !isJsonObject(value.when) ||
    (value.route !== undefined && typeof value.route !== 'string') ||
    (value.tags !== undefined && !isStringArray(value.tags)) ||
    Object.keys(value).some(
      (key) => !['id', 'when', 'route', 'tags'].includes(key)
    )
  )
    return false;
  return Object.entries(value.when).every(([key, field]) => {
    switch (key) {
      case 'operations':
      case 'modalities':
      case 'reasoning_effort':
        return isStringArray(field);
      case 'min_input_tokens':
      case 'max_input_tokens':
      case 'min_output_tokens':
      case 'max_output_tokens':
        return field === null || Number.isSafeInteger(field);
      case 'streaming':
      case 'tools':
      case 'structured_output':
        return field === null || typeof field === 'boolean';
      case 'classifier':
        return (
          field === null ||
          (isJsonObject(field) &&
            typeof field.route === 'string' &&
            isStringArray(field.labels) &&
            Number.isSafeInteger(field.timeout_ms) &&
            (field.min_score == null || typeof field.min_score === 'number') &&
            Object.keys(field).every((name) =>
              ['route', 'labels', 'timeout_ms', 'min_score'].includes(name)
            ))
        );
      case 'plugin':
        return (
          field === null ||
          (isJsonObject(field) &&
            typeof field.digest === 'string' &&
            Object.keys(field).every((name) => name === 'digest'))
        );
      default:
        return false;
    }
  });
}
