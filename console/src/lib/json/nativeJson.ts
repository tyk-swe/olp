/** JSON numbers in native documents retain their source spelling. The wrapper
 * is immutable and non-plain, so Svelte and query-cache structural sharing do
 * not turn the platform's raw JSON internal slot into an ordinary object. */
type SourceJSON = typeof JSON & {
  rawJSON(source: string): unknown;
  parse(
    source: string,
    reviver: (
      key: string,
      value: unknown,
      context: { source?: string }
    ) => unknown
  ): unknown;
};
const sourceJSON = JSON as SourceJSON;
const numberPattern = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;
const maxCharacters = 4 * 1024 * 1024;

export class NativeNumber {
  readonly source: string;
  constructor(source: string) {
    if (!numberPattern.test(source)) throw new Error('Invalid JSON number.');
    this.source = source;
    Object.freeze(this);
  }
  toJSON(): unknown {
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

/** Reject duplicate *decoded* names before JSON.parse can collapse them.
 * This bounded grammar check evaluates no code and delegates string decoding
 * to the platform JSON parser. In particular, __proto__ is ordinary JSON data. */
type SourceOrder = {
  keys?: string[];
  children: Map<string | number, SourceOrder>;
};
const memberOrders = new WeakMap<object, readonly string[]>();

function rememberOrder(value: object, keys: readonly string[]): void {
  memberOrders.set(value, Object.freeze([...keys]));
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

function checkUnambiguous(source: string): SourceOrder | undefined {
  if (
    source.length > maxCharacters ||
    new TextEncoder().encode(source).byteLength > maxCharacters
  )
    throw new Error('Native JSON must fit within 4 MiB.');
  let cursor = 0;
  let nodes = 0;
  const fail = (message = 'Invalid JSON'): never => {
    throw new Error(`${message} at character ${cursor + 1}.`);
  };
  const space = () => {
    while (/[\t\n\r ]/.test(source[cursor] ?? '\0')) cursor += 1;
  };
  const string = (): string => {
    const start = cursor++;
    while (cursor < source.length) {
      const character = source[cursor++];
      if (character === '\\') cursor += 1;
      else if (character === '"') {
        let decoded: string;
        try {
          decoded = JSON.parse(source.slice(start, cursor));
        } catch {
          return fail('Invalid JSON string');
        }
        if (/[\uD800-\uDFFF]/u.test(decoded))
          fail('Unpaired Unicode surrogate');
        return decoded;
      }
    }
    return fail('Unterminated JSON string');
  };
  const value = (depth: number): SourceOrder | undefined => {
    space();
    if (depth > 128 || ++nodes > 1_000_000)
      fail('Native JSON exceeds its nesting or value limit');
    const character = source[cursor];
    if (character === '"') {
      string();
      return;
    }
    if (character === '{' || character === '[') {
      const object = character === '{';
      const end = object ? '}' : ']';
      const names = new Set<string>();
      const order: SourceOrder = {
        keys: object ? [] : undefined,
        children: new Map()
      };
      let index = 0;
      cursor += 1;
      space();
      if (source[cursor] === end) {
        cursor += 1;
        return object ? order : undefined;
      }
      while (cursor < source.length) {
        let key: string | number = index++;
        if (object) {
          if (source[cursor] !== '"') fail('Expected an object member name');
          key = string();
          if (names.has(key)) fail('Duplicate object member');
          names.add(key);
          order.keys!.push(key);
          space();
          if (source[cursor++] !== ':') fail('Expected a colon');
        }
        const child = value(depth + 1);
        if (child) order.children.set(key, child);
        space();
        if (source[cursor] === end) {
          cursor += 1;
          return object || order.children.size ? order : undefined;
        }
        if (source[cursor++] !== ',') fail('Expected a comma');
        space();
      }
      fail('Unterminated JSON container');
    }
    const literal =
      /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(
        source.slice(cursor)
      )?.[0];
    if (!literal) fail();
    cursor += literal!.length;
  };
  const order = value(0);
  space();
  if (cursor !== source.length) fail('Expected one JSON document');
  return order;
}

function attachOrder(value: NativeValue, order: SourceOrder | undefined): void {
  if (!order || value === null || typeof value !== 'object') return;
  if (order.keys) rememberOrder(value, order.keys);
  for (const [key, child] of order.children) {
    const nested = Array.isArray(value)
      ? value[Number(key)]
      : (value as NativeObject)[String(key)];
    if (nested !== undefined) attachOrder(nested, child);
  }
}

export function parseNativeJSON(source: string): NativeValue {
  if (typeof sourceJSON.rawJSON !== 'function')
    throw new Error(
      'This browser does not support lossless native JSON editing.'
    );
  const order = checkUnambiguous(source);
  const parsed = sourceJSON.parse(
    source,
    (_key: string, value: unknown, context: { source?: string }) => {
      if (typeof value !== 'number') return value;
      if (!context?.source)
        throw new Error(
          'This browser cannot preserve native JSON number precision.'
        );
      return new NativeNumber(context.source);
    }
  ) as NativeValue;
  attachOrder(parsed, order);
  return parsed;
}

export function stringifyNativeJSON(value: unknown, indent = 0): string {
  return renderJSON(value, indent, false);
}

export function nativeObject(value: unknown): value is NativeObject {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    !(value instanceof NativeNumber) &&
    !(value instanceof IncompleteJSON)
  );
}

export function nativeAt(
  value: NativeValue,
  path: NativePath
): NativeValue | undefined {
  let current: NativeValue | undefined = value;
  for (const key of path) {
    if (!nativeObject(current) || !Object.hasOwn(current, key))
      return undefined;
    current = current[key];
  }
  return current;
}

/** Member replacement is atomic; arrays/schemas never recursively merge.
 * Undefined means explicit removal, while null is an ordinary native value. */
export function replaceNative(
  value: NativeValue,
  path: NativePath,
  next: NativeValue | undefined
): NativeValue {
  if (next === undefined && nativeAt(value, path) === undefined) return value;
  if (!path.length) {
    if (next === undefined)
      throw new Error('The root document cannot be removed.');
    return next;
  }
  const [key, ...rest] = path;
  const object: NativeObject = Object.create(null);
  if (nativeObject(value)) {
    for (const [name, entry] of nativeEntries(value))
      Object.defineProperty(object, name, {
        value: entry,
        writable: true,
        enumerable: true,
        configurable: true
      });
  } else if (value !== undefined && value !== null) {
    throw new Error('An object is required at this configuration path.');
  }
  if (rest.length) {
    const child = Object.hasOwn(object, key!) ? object[key!]! : null;
    Object.defineProperty(object, key!, {
      value: replaceNative(child, rest, next),
      writable: true,
      enumerable: true,
      configurable: true
    });
  } else if (next === undefined) delete object[key!];
  else
    Object.defineProperty(object, key!, {
      value: next,
      writable: true,
      enumerable: true,
      configurable: true
    });
  rememberOrder(object, [
    ...(nativeObject(value)
      ? nativeEntries(value)
          .map(([name]) => name)
          .filter((name) => Object.hasOwn(object, name))
      : []),
    ...Object.keys(object).filter(
      (name) => !nativeObject(value) || !Object.hasOwn(value, name)
    )
  ]);
  return object;
}

// The query cache's generic object copier assigns keys such as __proto__.
// Keep each complete native configuration opaque to structural sharing while
// retaining ordinary, read-only generated-contract properties for consumers.
class NativeConfiguration {
  [key: string]: NativeValue;
  constructor(value: NativeObject) {
    for (const [key, entry] of nativeEntries(value)) {
      Object.defineProperty(this, key, {
        value: freezeNative(entry),
        enumerable: true
      });
    }
    rememberOrder(
      this,
      nativeEntries(value).map(([key]) => key)
    );
    Object.freeze(this);
  }
}

/** Only provider configuration is a native editable document; pagination,
 * status and other generated management scalars retain their ordinary types. */
export function parseManagementJSON(source: string): unknown {
  const parsed = parseNativeJSON(source);
  const normalize = (value: NativeValue, native = false): unknown => {
    if (value instanceof NativeNumber)
      return native ? value : Number(value.source);
    if (Array.isArray(value))
      return value.map((item) => normalize(item, native));
    if (!nativeObject(value)) return value;
    const entries = nativeEntries(value).map(([key, child]) => {
      const configuration =
        key === 'configuration' &&
        nativeObject(child) &&
        Object.hasOwn(child, 'kind') &&
        Object.hasOwn(child, 'auth_mode');
      const decoded = normalize(child, native || configuration);
      return [
        key,
        configuration
          ? new NativeConfiguration(decoded as NativeObject)
          : decoded
      ] as const;
    });
    const result = Object.fromEntries(entries);
    rememberOrder(
      result,
      entries.map(([key]) => key)
    );
    return result;
  };
  return normalize(parsed);
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

export function nativeIssue(value: NativeValue): string | null {
  if (value instanceof IncompleteJSON) return value.issue;
  if (Array.isArray(value)) {
    for (const item of value) {
      const issue = nativeIssue(item);
      if (issue) return issue;
    }
  } else if (nativeObject(value)) {
    for (const item of Object.values(value)) {
      const issue = nativeIssue(item);
      if (issue) return issue;
    }
  }
  return null;
}

export function freezeNative(value: NativeValue): NativeValue {
  if (Array.isArray(value)) value.forEach(freezeNative);
  else if (nativeObject(value)) Object.values(value).forEach(freezeNative);
  if (value !== null && typeof value === 'object') Object.freeze(value);
  return value;
}

/** The editor may display an unfinished fragment. API serialization still
 * uses stringifyNativeJSON, whose incomplete sentinel always throws. */
export function renderNativeDraft(value: NativeValue, indentation = 2): string {
  return renderJSON(value, indentation, true);
}

function renderJSON(
  value: unknown,
  indentation: number,
  incomplete: boolean
): string {
  const ancestors = new Set<object>();
  const render = (node: unknown, depth: number): string => {
    if (node instanceof IncompleteJSON) {
      if (!incomplete) throw new Error(node.issue);
      return node.source;
    }
    if (node instanceof NativeNumber) return node.source;
    if (node === null || typeof node === 'string' || typeof node === 'boolean')
      return JSON.stringify(node);
    if (typeof node === 'number') {
      if (!Number.isFinite(node))
        throw new Error('Non-finite values are not JSON numbers.');
      return Object.is(node, -0) ? '-0' : String(node);
    }
    if (typeof node !== 'object') throw new Error('A JSON value is required.');
    if (ancestors.has(node))
      throw new Error('JSON cannot contain a cyclic reference.');
    ancestors.add(node);
    const padding = ' '.repeat(depth * indentation);
    const next = ' '.repeat((depth + 1) * indentation);
    const join = (open: string, close: string, entries: string[]) =>
      !entries.length
        ? open + close
        : indentation
          ? `${open}\n${entries.map((entry) => next + entry).join(',\n')}\n${padding}${close}`
          : open + entries.join(',') + close;
    let result: string;
    if (Array.isArray(node))
      result = join(
        '[',
        ']',
        Array.from(node, (item) =>
          item === undefined ? 'null' : render(item, depth + 1)
        )
      );
    else
      result = join(
        '{',
        '}',
        nativeEntries(node)
          .filter(([, entry]) => entry !== undefined)
          .map(
            ([key, entry]) =>
              `${JSON.stringify(key)}:${indentation ? ' ' : ''}${render(entry, depth + 1)}`
          )
      );
    ancestors.delete(node);
    return result;
  };
  return render(value, 0);
}
