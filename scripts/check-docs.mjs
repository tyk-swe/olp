import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { dirname, join, normalize, relative } from 'node:path';

const rootDocs = ['CHANGELOG.md', 'CONTRIBUTING.md', 'GLOSSARY.md', 'README.md', 'SECURITY.md'];
const docDirectories = ['docs', 'docs/develop', 'docs/operate', 'docs/use', 'docs/use/providers'];

export function listScopedDocs(listDirectory = (directory) => readdirSync(directory)) {
  const handbook = docDirectories.flatMap((directory) =>
    listDirectory(directory)
      .filter((entry) => entry.endsWith('.md'))
      .map((entry) => join(directory, entry))
  );
  return [...rootDocs, ...handbook].sort();
}

function stripInlineMarkdown(text) {
  return text
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\[[^\]]*\]/g, '$1')
    .replace(/<[^>]+>/g, '')
    .replace(/[`*_~]/g, '');
}

export function headingAnchor(text) {
  return stripInlineMarkdown(text)
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N} _-]/gu, '')
    .replace(/ /g, '-');
}

function withoutFencedCode(markdown) {
  const lines = markdown.split('\n');
  const visible = [];
  let fence = null;
  for (const line of lines) {
    const match = /^(\s*)(```+|~~~+)/.exec(line);
    if (match && (!fence || line.trimStart().startsWith(fence))) {
      fence = fence ? null : match[2];
      visible.push('');
      continue;
    }
    visible.push(fence ? '' : line);
  }
  return visible.join('\n');
}

function withoutInlineCode(markdown) {
  return markdown.replace(/`[^`\n]*`/g, (code) => ' '.repeat(code.length));
}

export function collectAnchors(markdown) {
  const visible = withoutFencedCode(markdown);
  const anchors = new Set();
  const counts = new Map();
  for (const line of visible.split('\n')) {
    const heading = /^#{1,6}\s+(.+?)\s*#*\s*$/.exec(line);
    if (heading) {
      const base = headingAnchor(heading[1]);
      const count = counts.get(base) ?? 0;
      counts.set(base, count + 1);
      anchors.add(count === 0 ? base : `${base}-${count}`);
    }
    for (const match of line.matchAll(/<a\s+[^>]*\bid\s*=\s*["']([^"']+)["']/gi)) {
      anchors.add(match[1]);
    }
  }
  return anchors;
}

function linkDestination(raw) {
  const trimmed = raw.trim();
  if (trimmed.startsWith('<')) {
    const end = trimmed.indexOf('>');
    return end < 0 ? trimmed.slice(1) : trimmed.slice(1, end);
  }
  return trimmed.split(/\s+/, 1)[0];
}

export function collectLinks(markdown) {
  const visible = withoutInlineCode(withoutFencedCode(markdown));
  const definitions = new Map();
  for (const match of visible.matchAll(/^\s*\[([^\]]+)\]:\s*(\S+)/gm)) {
    definitions.set(match[1].toLowerCase(), linkDestination(match[2]));
  }

  const links = [];
  for (const match of visible.matchAll(/!?\[[^\]\n]*\]\(([^)\n]+)\)/g)) {
    links.push(linkDestination(match[1]));
  }
  for (const match of visible.matchAll(/!?\[[^\]\n]+\]\[([^\]\n]+)\]/g)) {
    const destination = definitions.get(match[1].toLowerCase());
    if (destination) links.push(destination);
  }
  for (const match of visible.matchAll(/<a\s+[^>]*\bhref\s*=\s*["']([^"']+)["']/gi)) {
    links.push(match[1]);
  }
  return links;
}

function isExternal(destination) {
  return /^[a-z][a-z0-9+.-]*:/i.test(destination) || destination.startsWith('//');
}

function resolveLink(source, destination) {
  const hash = destination.indexOf('#');
  const pathPart = hash < 0 ? destination : destination.slice(0, hash);
  const anchor = hash < 0 ? '' : destination.slice(hash + 1);
  const decodedPath = decodeURIComponent(pathPart);
  const target = decodedPath ? normalize(join(dirname(source), decodedPath)) : source;
  return { anchor: decodeURIComponent(anchor), target };
}

export function checkDocs(files, read, exists) {
  const errors = [];
  const anchorsByFile = new Map();

  for (const file of files) {
    const markdown = read(file);
    if (typeof markdown !== 'string') {
      errors.push(`${file}: scoped documentation file is missing`);
      continue;
    }
    anchorsByFile.set(file, collectAnchors(markdown));
    if (markdown.includes('—')) errors.push(`${file}: contains an em dash`);
  }

  for (const file of files) {
    const markdown = read(file);
    if (typeof markdown !== 'string') continue;
    for (const destination of collectLinks(markdown)) {
      if (!destination || isExternal(destination)) continue;
      const { anchor, target } = resolveLink(file, destination);
      if (!exists(target)) {
        errors.push(`${file}: broken local link ${destination}`);
        continue;
      }
      if (anchor && target.endsWith('.md')) {
        const anchors = anchorsByFile.get(target) ?? collectAnchors(read(target) ?? '');
        if (!anchors.has(anchor)) errors.push(`${file}: broken local anchor ${destination}`);
      }
    }
  }

  const index = 'docs/README.md';
  const indexMarkdown = read(index);
  if (typeof indexMarkdown === 'string') {
    const indexed = new Set(
      collectLinks(indexMarkdown)
        .filter((destination) => destination && !isExternal(destination))
        .map((destination) => resolveLink(index, destination).target)
    );
    for (const file of files) {
      if (file.startsWith('docs/') && file !== index && !indexed.has(file)) {
        errors.push(`${index}: missing handbook link for ${relative('docs', file)}`);
      }
    }
  }

  if (errors.length > 0) throw new Error(errors.join('\n'));
  return files.length;
}

if (import.meta.main) {
  const files = listScopedDocs();
  const count = checkDocs(
    files,
    (file) => (existsSync(file) ? readFileSync(file, 'utf8') : undefined),
    (file) => existsSync(file)
  );
  console.log(`Documentation check: ${count} files`);
}
