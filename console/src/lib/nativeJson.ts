/** JSON numbers in native documents retain their source spelling. The wrapper
 * is immutable and non-plain, so Svelte and query-cache structural sharing do
 * not turn the platform's raw JSON internal slot into an ordinary object. */
type SourceJSON = typeof JSON & {
  rawJSON?: (source: string) => unknown;
};
const sourceJSON = JSON as SourceJSON;
const utf8 = new TextEncoder();
const numberPattern = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;
const literalPattern =
  /true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
const unpairedSurrogatePattern = /[\uD800-\uDFFF]/u;
const defaultMaxBytes = 4 * 1024 * 1024;

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

const memberOrders = new WeakMap<object, readonly string[]>();

function rememberOrder(value: object, keys: readonly string[]): void {
  memberOrders.set(value, Object.freeze([...keys]));
}

function defineDataProperty(
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

function cloneNativeTree(
  value: unknown,
  options: { wrapNumbers: boolean; omitUndefined: boolean }
): unknown {
  const ancestors = new Set<object>();
  const copy = (node: unknown): unknown => {
    if (node === null || typeof node === 'string' || typeof node === 'boolean')
      return node;
    if (typeof node === 'number') {
      if (!Number.isFinite(node))
        throw new Error('Non-finite values are not JSON numbers.');
      return options.wrapNumbers
        ? new NativeNumber(Object.is(node, -0) ? '-0' : String(node))
        : node;
    }
    if (node === undefined) return undefined;
    if (typeof node !== 'object')
      throw new Error('A JSON submission must contain JSON values.');
    if (node instanceof NativeNumber || node instanceof IncompleteJSON)
      return node;
    if (ancestors.has(node))
      throw new Error('JSON cannot contain a cyclic reference.');
    ancestors.add(node);
    try {
      if (Array.isArray(node)) {
        return Object.freeze(
          Array.from(node, (item) =>
            options.omitUndefined && item === undefined ? null : copy(item)
          )
        );
      }
      const prototype = Object.getPrototypeOf(node);
      if (
        prototype !== Object.prototype &&
        prototype !== null &&
        !(node instanceof NativeConfiguration)
      ) {
        throw new Error('A JSON submission must contain JSON values.');
      }
      const entries = nativeEntries(node).filter(
        ([, entry]) => !options.omitUndefined || entry !== undefined
      );
      const result: Record<string, unknown> = {};
      for (const [key, entry] of entries) {
        defineDataProperty(result, key, copy(entry));
      }
      rememberOrder(
        result,
        entries.map(([key]) => key)
      );
      return Object.freeze(result);
    } finally {
      ancestors.delete(node);
    }
  };
  return copy(value);
}

/** Own a JSON submission without coercing native number spellings or losing
 * remembered object-member order. Immutable number wrappers are safe to share. */
export function snapshotNativeJSON<T>(value: T): T {
  return cloneNativeTree(value, {
    wrapNumbers: false,
    omitUndefined: false
  }) as T;
}

/** Clone a plain or native value into a frozen native JSON tree, preserving
 * member order and wrapping finite numbers as exact NativeNumber tokens. */
export function normalizeNativeJSON(value: unknown): NativeValue {
  if (value === undefined) throw new Error('A JSON value is required.');
  return cloneNativeTree(value, {
    wrapNumbers: true,
    omitUndefined: true
  }) as NativeValue;
}

/** Single-pass bounded RFC 8259 parser that preserves exact numeric tokens and
 * object member order while rejecting duplicate decoded member names. */
export function parseNativeJSON(
  source: string,
  maxBytes = defaultMaxBytes
): NativeValue {
  if (source.length > maxBytes || utf8.encode(source).byteLength > maxBytes)
    throw new Error(`Native JSON must fit within ${maxBytes / 1024 ** 2} MiB.`);

  let cursor = 0;
  let nodes = 0;

  const fail = (message = 'Invalid JSON'): never => {
    throw new Error(`${message} at character ${cursor + 1}.`);
  };

  const skipSpace = (): void => {
    while (cursor < source.length) {
      const code = source.charCodeAt(cursor);
      if (code === 0x20 || code === 0x09 || code === 0x0a || code === 0x0d) {
        cursor += 1;
      } else {
        break;
      }
    }
  };

  const readString = (): string => {
    const start = cursor++;
    let hasEscape = false;
    while (cursor < source.length) {
      const code = source.charCodeAt(cursor++);
      if (code === 0x5c) {
        hasEscape = true;
        cursor += 1;
        if (cursor > source.length) return fail('Unterminated JSON string');
      } else if (code === 0x22) {
        let decoded: string;
        if (hasEscape) {
          try {
            decoded = JSON.parse(source.slice(start, cursor)) as string;
          } catch {
            return fail('Invalid JSON string');
          }
        } else {
          decoded = source.slice(start + 1, cursor - 1);
        }
        if (unpairedSurrogatePattern.test(decoded))
          fail('Unpaired Unicode surrogate');
        return decoded;
      } else if (code < 0x20) {
        return fail('Invalid JSON string');
      }
    }
    return fail('Unterminated JSON string');
  };

  const readObject = (depth: number): NativeObject => {
    cursor += 1;
    skipSpace();
    const result: NativeObject = {};
    const keys: string[] = [];
    const seen = new Set<string>();
    if (source.charCodeAt(cursor) === 0x7d) {
      cursor += 1;
      rememberOrder(result, keys);
      return result;
    }
    while (cursor < source.length) {
      if (source.charCodeAt(cursor) !== 0x22)
        fail('Expected an object member name');
      const key = readString();
      if (seen.has(key)) fail('Duplicate object member');
      seen.add(key);
      keys.push(key);
      skipSpace();
      if (source.charCodeAt(cursor++) !== 0x3a) fail('Expected a colon');
      const child = readValue(depth + 1);
      defineDataProperty(result, key, child);
      skipSpace();
      const delimiter = source.charCodeAt(cursor);
      if (delimiter === 0x7d) {
        cursor += 1;
        rememberOrder(result, keys);
        return result;
      }
      if (delimiter !== 0x2c) fail('Expected a comma');
      cursor += 1;
      skipSpace();
    }
    return fail('Unterminated JSON container');
  };

  const readArray = (depth: number): NativeValue[] => {
    cursor += 1;
    skipSpace();
    const items: NativeValue[] = [];
    if (source.charCodeAt(cursor) === 0x5d) {
      cursor += 1;
      return items;
    }
    while (cursor < source.length) {
      items.push(readValue(depth + 1));
      skipSpace();
      const delimiter = source.charCodeAt(cursor);
      if (delimiter === 0x5d) {
        cursor += 1;
        return items;
      }
      if (delimiter !== 0x2c) fail('Expected a comma');
      cursor += 1;
      skipSpace();
    }
    return fail('Unterminated JSON container');
  };

  const readLiteral = (): NativeValue => {
    literalPattern.lastIndex = cursor;
    const match = literalPattern.exec(source);
    if (!match) return fail();
    const token = match[0];
    cursor += token.length;
    if (token === 'true') return true;
    if (token === 'false') return false;
    if (token === 'null') return null;
    return new NativeNumber(token);
  };

  const readValue = (depth: number): NativeValue => {
    skipSpace();
    if (depth > 128 || ++nodes > 1_000_000)
      fail('Native JSON exceeds its nesting or value limit');
    const code = source.charCodeAt(cursor);
    if (code === 0x22) return readString();
    if (code === 0x7b) return readObject(depth);
    if (code === 0x5b) return readArray(depth);
    return readLiteral();
  };

  const parsed = readValue(0);
  skipSpace();
  if (cursor !== source.length) fail('Expected one JSON document');
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
      defineDataProperty(object, name, entry);
  } else if (value !== undefined && value !== null) {
    throw new Error('An object is required at this configuration path.');
  }
  if (rest.length) {
    const child = Object.hasOwn(object, key!) ? object[key!]! : null;
    defineDataProperty(object, key!, replaceNative(child, rest, next));
  } else if (next === undefined) delete object[key!];
  else defineDataProperty(object, key!, next);
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

export function parseManagementJSON(
  source: string,
  nativePaths: readonly NativePath[] = [],
  maxBytes?: number
): unknown {
  const normalize = (
    value: NativeValue,
    path: readonly (string | number)[]
  ): unknown => {
    if (
      nativePaths.some(
        (nativePath) =>
          nativePath.length === path.length &&
          nativePath.every(
            (part, index) =>
              part === path[index] ||
              (part === '*' && typeof path[index] === 'number')
          )
      )
    )
      return nativeObject(value)
        ? new NativeConfiguration(value)
        : freezeNative(value);
    if (value instanceof NativeNumber) return Number(value.source);
    if (Array.isArray(value))
      return value.map((item, index) => normalize(item, [...path, index]));
    if (!nativeObject(value)) return value;
    const entries = nativeEntries(value).map(
      ([key, child]) => [key, normalize(child, [...path, key])] as const
    );
    const result = Object.fromEntries(entries);
    rememberOrder(
      result,
      entries.map(([key]) => key)
    );
    return result;
  };
  return normalize(parseNativeJSON(source, maxBytes), []);
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
export function renderNativeDraft(value: NativeValue): string {
  return renderJSON(value, 2, true);
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
