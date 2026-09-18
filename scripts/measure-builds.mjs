#!/usr/bin/env node
// Each case runs in a disposable source snapshot. Never mutate the user's tree.
import { spawnSync } from 'node:child_process';
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync
} from 'node:fs';
import {
  cpus,
  freemem,
  platform,
  release,
  tmpdir,
  totalmem,
  arch
} from 'node:os';
import { resolve, join, dirname } from 'node:path';
import { createHash } from 'node:crypto';
import { performance } from 'node:perf_hooks';

const options = Object.fromEntries(
  process.argv.slice(2).map((arg) => {
    const [name, ...value] = arg.replace(/^--/, '').split('=');
    return [name, value.join('=')];
  })
);
const language = options.language ?? 'go';
const scenario = options.case ?? 'backend-clean';
const count = Number(options.runs ?? 5);
if (!['rust', 'go'].includes(language) || !Number.isInteger(count) || count < 5)
  throw new Error('Use --language=rust|go and --runs=N (at least 5)');
const root = process.cwd();
const output = resolve(
  options.output ?? `.local/measurements/${language}-${scenario}`
);
mkdirSync(output, { recursive: true });
const scratch = mkdtempSync(join(tmpdir(), 'olp-build-'));
const source = join(scratch, 'source');
mkdirSync(source);
function command(argv, cwd = source, env = {}) {
  const start = performance.now();
  const result = spawnSync(argv[0], argv.slice(1), {
    cwd,
    env: { ...process.env, ...env },
    encoding: 'utf8',
    maxBuffer: 128 * 1024 * 1024
  });
  return {
    command: argv,
    seconds: (performance.now() - start) / 1000,
    status: result.status,
    stdout: result.stdout ?? '',
    stderr: result.stderr ?? '',
    error: result.error?.message
  };
}
function required(argv, cwd, env) {
  const result = command(argv, cwd, env);
  if (result.status !== 0)
    throw new Error(
      `${argv.join(' ')} failed: ${result.error ?? result.stderr}`
    );
  return result;
}
const ref =
  options.ref ??
  (language === 'rust'
    ? '6c21dfb917c9019161348ea24b532a77b6612e6e'
    : 'working');
const data = {
  language,
  scenario,
  ref,
  started: new Date().toISOString(),
  machine: {
    platform: platform(),
    release: release(),
    arch: arch(),
    cpu: cpus()[0]?.model,
    logicalCPUs: cpus().length,
    memoryBytes: totalmem(),
    freeMemoryBytes: freemem()
  },
  cache: {
    downloads:
      scenario === 'image'
        ? 'host metadata prefetched separately; downloads inside Docker included'
        : 'prefetched; measured separately',
    compilation:
      scenario === 'backend-clean'
        ? 'empty private cache for every run'
        : scenario === 'image'
          ? 'Docker layer reuse disabled; explicit BuildKit Go cache mount retained'
          : 'private cache populated by explicit warmup',
    sharedOSPageCache: true
  },
  runs: []
};
function save() {
  const successes = data.runs
    .filter((r) => r.status === 0)
    .map((r) => r.seconds)
    .sort((a, b) => a - b);
  data.summary =
    successes.length < 5
      ? { status: 'unmeasured', successfulRuns: successes.length }
      : {
          successfulRuns: successes.length,
          medianSeconds:
            (successes[Math.floor((successes.length - 1) / 2)] +
              successes[Math.floor(successes.length / 2)]) /
            2,
          minSeconds: successes[0],
          maxSeconds: successes.at(-1)
        };
  writeFileSync(
    join(output, 'results.json'),
    `${JSON.stringify(data, null, 2)}\n`
  );
}
try {
  if (ref === 'working') {
    const files = required(
      ['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'],
      root
    )
      .stdout.split('\0')
      .filter(Boolean);
    const digest = createHash('sha256');
    for (const file of files.sort()) {
      if (!existsSync(join(root, file))) continue;
      mkdirSync(dirname(join(source, file)), { recursive: true });
      cpSync(join(root, file), join(source, file));
      digest
        .update(file)
        .update('\0')
        .update(readFileSync(join(root, file)));
    }
    data.sourceSHA256 = digest.digest('hex');
    data.baseCommit = required(
      ['git', 'rev-parse', 'HEAD'],
      root
    ).stdout.trim();
  } else {
    required(
      [
        'git',
        'archive',
        '--format=tar',
        `--output=${join(scratch, 'source.tar')}`,
        ref
      ],
      root
    );
    required(['tar', '-xf', join(scratch, 'source.tar'), '-C', source], root);
    data.commit = required(['git', 'rev-parse', ref], root).stdout.trim();
  }
  const env =
    language === 'rust'
      // Keep measured Rust artifacts in the disposable source tree.
      ? { CARGO_TARGET_DIR: join(source, 'target') }
      : { GOCACHE: join(scratch, 'cache'), CGO_ENABLED: '1' };
  data.toolchains = Object.fromEntries(
    (language === 'rust'
      ? [
          ['rustc', '--version'],
          ['cargo', '--version'],
          ['cc', '--version']
        ]
      : [
          ['go', 'version'],
          ['cc', '--version'],
          ['node', '--version'],
          ['pnpm', '--version']
        ]
    ).map((argv) => [argv[0], required(argv).stdout.trim()])
  );
  data.prefetch = required(
    language === 'rust'
      ? ['cargo', 'fetch', '--locked']
      : ['go', 'mod', 'download'],
    source,
    env
  );
  if (language === 'rust') {
    writeFileSync(
      join(output, 'dependencies.json'),
      required(
        ['cargo', 'metadata', '--locked', '--format-version=1'],
        source,
        env
      ).stdout
    );
    writeFileSync(
      join(output, 'dependencies.txt'),
      required(['cargo', 'tree', '--locked', '--edges=all'], source, env).stdout
    );
  } else {
    for (const [name, argv] of Object.entries({
      modules: ['go', 'list', '-m', '-json', 'all'],
      production: ['go', 'list', '-deps', '-json', './cmd/olp'],
      tests: ['go', 'list', '-deps', '-test', '-json', './internal/...'],
      tools: [
        'go',
        'list',
        '-deps',
        '-json',
        'github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen'
      ]
    })) {
      writeFileSync(
        join(output, `${name}.jsonl`),
        required(argv, source, env).stdout
      );
    }
    writeFileSync(
      join(output, 'module-graph.txt'),
      required(['go', 'mod', 'graph'], source, env).stdout
    );
  }
  const backend =
    language === 'rust'
      ? ['cargo', 'build', '--locked', '--bin', 'olp']
      : ['go', 'build', '-trimpath', '-o', join(scratch, 'olp'), './cmd/olp'];
  const cases = {
    'backend-clean': backend,
    'backend-edit': backend,
    'targeted-test':
      language === 'rust'
        ? ['cargo', 'test', '--locked', '--test', 'sse']
        : ['go', 'test', '-count=1', './internal/protocols/sse'],
    api: ['make', 'api'],
    console: ['pnpm', '--dir', 'console', 'build'],
    check: ['make', 'check'],
    integration: [
      'make',
      'integration'
    ],
    image: [
      'docker',
      'build',
      '--no-cache',
      '-f',
      'deploy/Dockerfile',
      '.'
    ]
  };
  const argv = cases[scenario];
  if (!argv) throw new Error(`Unknown --case=${scenario}`);
  if (['api', 'console', 'check', 'integration'].includes(scenario)) {
    data.consolePrefetch = required(
      ['pnpm', 'install', '--frozen-lockfile'],
      source,
      env
    );
    if (scenario === 'console')
      required(['make', 'api'], source, env);
  }
  if (scenario !== 'backend-clean') data.warmup = required(argv, source, env);
  const editFile = join(
    source,
    language === 'rust' ? 'src/process/cli.rs' : 'internal/process/listener.go'
  );
  const original = existsSync(editFile) ? readFileSync(editFile, 'utf8') : '';
  for (let i = 0; i < count; i++) {
    if (scenario === 'backend-clean')
      rmSync(language === 'rust' ? env.CARGO_TARGET_DIR : env.GOCACHE, {
        recursive: true,
        force: true
      });
    if (scenario === 'backend-edit') {
      const edited =
        language === 'rust'
          ? original.replace(
              'BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5)',
              `BACKGROUND_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(${6 + i})`
            )
          : original.replace(
              /ReadHeaderTimeout:\s*5\s*\*\s*time.Second/,
              `ReadHeaderTimeout: ${6 + i} * time.Second`
            );
      if (edited === original)
        throw new Error(
          'Implementation edit did not match; refusing a no-op rebuild measurement'
        );
      writeFileSync(editFile, edited);
      data.edit = {
        file: editFile.replace(`${source}/`, ''),
        description:
          language === 'rust'
            ? 'Increase background shutdown timeout on every run'
            : 'Increase HTTP read-header timeout on every run'
      };
    }
    const result = command(argv, source, env);
    writeFileSync(
      join(output, `run-${i + 1}.log`),
      result.stdout + result.stderr
    );
    data.runs.push({
      command: result.command,
      seconds: result.seconds,
      status: result.status,
      error: result.error,
      log: `run-${i + 1}.log`
    });
    save();
    console.log(
      `${language} ${scenario} ${i + 1}/${count}: ${result.seconds.toFixed(3)}s (status ${result.status})`
    );
    if (result.status !== 0)
      throw new Error(`Measurement failed; see ${output}/run-${i + 1}.log`);
  }
} catch (error) {
  data.failure = error.message;
  process.exitCode = 1;
} finally {
  data.finished = new Date().toISOString();
  save();
  rmSync(scratch, { recursive: true, force: true });
}
