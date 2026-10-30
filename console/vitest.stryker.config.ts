import { fileURLToPath } from 'node:url';
import { mergeConfig } from 'vite';
import config from './vite.config.ts';

const goldenImport =
  '../../../../../../../internal/access/testdata/authorization.golden.json';

export default process.env.STRYKER_MUTATOR_WORKER === undefined
  ? config
  : mergeConfig(config, {
      resolve: {
        alias: {
          [goldenImport]: fileURLToPath(
            new URL(
              '../../../internal/access/testdata/authorization.golden.json',
              import.meta.url
            )
          )
        }
      }
    });
