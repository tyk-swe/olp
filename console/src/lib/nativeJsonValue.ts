/** JSON numbers in native documents retain their source spelling. The wrapper
 * is immutable and non-plain, so Svelte and query-cache structural sharing do
 * not turn the platform's raw JSON internal slot into an ordinary object. */
type SourceJSON = typeof JSON & {
  rawJSON?: (source: string) => unknown;
};
const sourceJSON = JSON as SourceJSON;
const numberPattern = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;

export class NativeNumber {
  readonly source: string;
  constructor(source: string) {
    if (!numberPattern.test(source)) throw new Error('Invalid JSON number.');
    this.source = source;
    Object.freeze(this);
  }
  toJSON(): unknown {
    if (typeof sourceJSON.rawJSON !== 'function')
      throw new Error(
        'NativeNumber cannot be serialized with JSON.stringify in this runtime; use stringifyNativeJSON.'
      );
    return sourceJSON.rawJSON(this.source);
  }
  toString(): string {
    return this.source;
  }
  valueOf(): never {
    throw new Error(
      'Native numbers must be edited as JSON without numeric coercion.'
    );
  }
}

/** An unfinished field edit is part of the one draft tree, never a second
 * unsynchronized form value. It can be rendered but cannot be sent as JSON. */
export class IncompleteJSON {
  constructor(
    readonly source: string,
    readonly issue: string
  ) {
    Object.freeze(this);
  }
  toJSON(): never {
    throw new Error(this.issue);
  }
}

export type NativeValue =
  | null
  | boolean
  | string
  | number
  | NativeNumber
  | IncompleteJSON
  | NativeValue[]
  | NativeObject;
export type NativeObject = { [name: string]: NativeValue };
export type NativePath = readonly string[];

export function nativeObject(value: unknown): value is NativeObject {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    !(value instanceof NativeNumber) &&
    !(value instanceof IncompleteJSON)
  );
}

const memberOrders = new WeakMap<object, readonly string[]>();

export function rememberOrder(value: object, keys: readonly string[]): void {
  memberOrders.set(value, Object.freeze([...keys]));
}

export function defineDataProperty(
  target: Record<string, unknown>,
  key: string,
  value: unknown
): void {
  Object.defineProperty(target, key, {
    value,
    writable: true,
    enumerable: true,
    configurable: true
  });
}

export function nativeEntries(value: object): [string, NativeValue][] {
  const current = Object.keys(value);
  const remembered = memberOrders.get(value);
  const rememberedKeys = new Set(remembered);
  const keys = remembered
    ? [
        ...remembered.filter((key) => Object.hasOwn(value, key)),
        ...current.filter((key) => !rememberedKeys.has(key))
      ]
    : current;
  return keys.map((key) => [key, (value as NativeObject)[key]!]);
}
