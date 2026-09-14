import type { components } from '$lib/api/schema';

type Schemas = components['schemas'];

export type ApiKeyBudget = Schemas['ApiKeyBudgetResponse'];
export type BudgetWindowName = 'daily' | 'monthly';

/**
 * What the console is allowed to claim about one budget window.
 *
 * `policy` means the amounts are configuration rather than accounting, so an
 * accrued amount of zero says nothing about spend. `unknown` keeps the console
 * from ordering amounts it cannot compare exactly; it never claims headroom it
 * has not proven.
 */
export type BudgetState =
  'policy' | 'unlimited' | 'within' | 'exhausted' | 'unknown';

const DECIMAL = /^\d+(?:\.\d+)?$/;

function split(value: string): [string, string] | null {
  if (!DECIMAL.test(value)) return null;
  const [integer, fraction = ''] = value.split('.');
  return [integer.replace(/^0+(?=\d)/, ''), fraction];
}

/**
 * Orders two exact non-negative decimal strings, or returns null when either
 * side is not one.
 *
 * Accrued spend and budget limits are exact decimals with more precision than a
 * double carries, so parsing them as numbers would decide "exhausted" from a
 * rounded value. Comparison stays on the digits the server actually sent.
 */
export function compareDecimalStrings(
  left: string,
  right: string
): number | null {
  const first = split(left);
  const second = split(right);
  if (!first || !second) return null;
  if (first[0].length !== second[0].length)
    return first[0].length < second[0].length ? -1 : 1;
  if (first[0] !== second[0]) return first[0] < second[0] ? -1 : 1;
  const width = Math.max(first[1].length, second[1].length);
  const firstFraction = first[1].padEnd(width, '0');
  const secondFraction = second[1].padEnd(width, '0');
  if (firstFraction === secondFraction) return 0;
  return firstFraction < secondFraction ? -1 : 1;
}

/** The state of one window of a key's cost budget. */
export function budgetWindowState(
  budget: ApiKeyBudget,
  name: BudgetWindowName
): BudgetState {
  // The field is optional, so only an explicit false means the amounts are
  // stored policy instead of live accounting.
  if (budget.enforcement_active === false) return 'policy';
  const window = budget[name];
  if (window.limit === null) return 'unlimited';
  const order = compareDecimalStrings(window.accrued, window.limit);
  if (order === null) return 'unknown';
  // The gateway refuses a request once the window has reached its limit, so
  // equality is already exhaustion rather than the last spendable moment.
  return order < 0 ? 'within' : 'exhausted';
}

const WORST_FIRST: BudgetState[] = ['policy', 'exhausted', 'unknown', 'within'];

/** The state of a key's whole budget: the most restrictive of its windows. */
export function budgetState(budget: ApiKeyBudget): BudgetState {
  const windows = [
    budgetWindowState(budget, 'daily'),
    budgetWindowState(budget, 'monthly')
  ];
  return WORST_FIRST.find((state) => windows.includes(state)) ?? 'unlimited';
}

/** A badge label for a budget state, for inventories that show one per key. */
export function budgetStateLabel(state: BudgetState): string {
  switch (state) {
    case 'policy':
      return 'Saved policy · not enforced';
    case 'exhausted':
      return 'Budget exhausted';
    case 'unknown':
      return 'Budget state unknown';
    case 'within':
      return 'Within budget';
    case 'unlimited':
      return 'No cost budget';
  }
}

/** The sentence a key or usage view shows under an unusual budget state. */
export function budgetStateNote(state: BudgetState): string | null {
  switch (state) {
    case 'policy':
      return 'Accrued amounts are stored policy on this installation, not live spend.';
    case 'exhausted':
      return 'Requests with this key are refused until the window resets.';
    case 'unknown':
      return 'The server reported an amount this console cannot compare, so remaining budget is unknown.';
    default:
      return null;
  }
}
