import {
  NativeNumber,
  defineDataProperty,
  rememberOrder,
  type NativeObject,
  type NativeValue
} from './nativeJsonValue.js';

const utf8 = new TextEncoder();
const literalPattern =
  /true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
const unpairedSurrogatePattern = /[\uD800-\uDFFF]/u;
const defaultMaxBytes = 4 * 1024 * 1024;
const maxDepth = 128;
const maxValues = 1_000_000;

const isSpace = (code: number): boolean =>
  code === 0x20 || code === 0x09 || code === 0x0a || code === 0x0d;
const quote = 0x22;
const comma = 0x2c;
const colon = 0x3a;
const openBracket = 0x5b;
const backslash = 0x5c;
const closeBracket = 0x5d;
const openBrace = 0x7b;
const closeBrace = 0x7d;

class JSONReader {
  #position = 0;
  #values = 0;

  constructor(private readonly source: string) {}

  document(): NativeValue {
    const parsed = this.#value(0);
    this.#skipSpace();
    if (this.#position !== this.source.length)
      this.#fail('Expected one JSON document');
    return parsed;
  }

  #fail(message = 'Invalid JSON'): never {
    throw new Error(`${message} at character ${this.#position + 1}.`);
  }

  #peek(): number {
    return this.source.charCodeAt(this.#position);
  }

  #take(): number {
    const code = this.#peek();
    this.#position += 1;
    return code;
  }

  #accept(code: number): boolean {
    if (this.#peek() !== code) return false;
    this.#position += 1;
    return true;
  }

  #skipSpace(): void {
    while (isSpace(this.#peek())) this.#position += 1;
  }

  #value(depth: number): NativeValue {
    this.#skipSpace();
    this.#values += 1;
    if (depth > maxDepth || this.#values > maxValues)
      this.#fail('Native JSON exceeds its nesting or value limit');
    const code = this.#peek();
    if (code === quote) return this.#string();
    if (code === openBrace) return this.#object(depth);
    if (code === openBracket) return this.#array(depth);
    return this.#literal();
  }

  #literal(): NativeValue {
    literalPattern.lastIndex = this.#position;
    const token = literalPattern.exec(this.source)?.[0];
    if (token === undefined) return this.#fail();
    this.#position += token.length;
    if (token === 'null') return null;
    if (token === 'true' || token === 'false') return token === 'true';
    return new NativeNumber(token);
  }

  #string(): string {
    const start = this.#position;
    this.#passString();
    const body = this.source.slice(start + 1, this.#position - 1);
    const decoded = body.includes('\\') ? this.#unescape(start) : body;
    if (unpairedSurrogatePattern.test(decoded))
      this.#fail('Unpaired Unicode surrogate');
    return decoded;
  }

  #passString(): void {
    this.#position += 1;
    while (this.#position < this.source.length) {
      const code = this.#take();
      if (code === quote) return;
      if (code === backslash) {
        this.#position += 1;
        if (this.#position > this.source.length) break;
      } else if (code < 0x20) this.#fail('Invalid JSON string');
    }
    this.#fail('Unterminated JSON string');
  }

  #unescape(start: number): string {
    try {
      return JSON.parse(this.source.slice(start, this.#position)) as string;
    } catch {
      return this.#fail('Invalid JSON string');
    }
  }

  #container(close: number, readEntry: () => void): void {
    this.#position += 1;
    this.#skipSpace();
    if (this.#accept(close)) return;
    while (this.#position < this.source.length) {
      readEntry();
      this.#skipSpace();
      if (this.#accept(close)) return;
      if (!this.#accept(comma)) this.#fail('Expected a comma');
      this.#skipSpace();
    }
    this.#fail('Unterminated JSON container');
  }

  #object(depth: number): NativeObject {
    const result: NativeObject = {};
    const keys = new Set<string>();
    this.#container(closeBrace, () => {
      if (this.#peek() !== quote) this.#fail('Expected an object member name');
      const key = this.#string();
      if (keys.has(key)) this.#fail('Duplicate object member');
      keys.add(key);
      this.#skipSpace();
      if (this.#take() !== colon) this.#fail('Expected a colon');
      defineDataProperty(result, key, this.#value(depth + 1));
    });
    rememberOrder(result, [...keys]);
    return result;
  }

  #array(depth: number): NativeValue[] {
    const items: NativeValue[] = [];
    this.#container(closeBracket, () => items.push(this.#value(depth + 1)));
    return items;
  }
}

export function parseNativeJSON(
  source: string,
  maxBytes = defaultMaxBytes
): NativeValue {
  if (source.length > maxBytes || utf8.encode(source).byteLength > maxBytes)
    throw new Error(`Native JSON must fit within ${maxBytes / 1024 ** 2} MiB.`);
  return new JSONReader(source).document();
}
