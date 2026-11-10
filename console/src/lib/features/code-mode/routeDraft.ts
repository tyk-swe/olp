import { validRouteSlug } from '#lib/routeIdentity.js';
import type {
  CodeAccount,
  CodeRouteWrite
} from '#lib/features/code-mode/api.js';
import { parseNativeModels, type Validation } from './presentation';

export type PaidTargets = NonNullable<CodeRouteWrite['paid_targets']>;
export type ResearchService = 'search' | 'reader' | 'zread';

export function parseAliases(
  value: string,
  models: readonly string[],
  rewriting: boolean
): Validation<Record<string, string>> {
  const entries = value
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => line.split('=').map((part) => part.trim()));
  if (
    !entries.every(
      (entry): entry is [string, string] =>
        entry.length === 2 &&
        !!entry[0] &&
        !!entry[1] &&
        /^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$/.test(entry[0]) &&
        !models.includes(entry[0]) &&
        models.includes(entry[1])
    )
  ) {
    return {
      ok: false,
      message:
        'Use one distinct alias=native-model per line, targeting a listed native model.'
    };
  }
  if (entries.length && !rewriting) {
    return {
      ok: false,
      message: 'Enable model rewriting to publish aliases.'
    };
  }
  if (
    entries.length > 100 ||
    new Set(entries.map(([alias]) => alias)).size !== entries.length
  ) {
    return { ok: false, message: 'Use up to 100 unique aliases.' };
  }
  return { ok: true, value: Object.fromEntries(entries) };
}

export function parseModelPools(
  value: string,
  models: readonly string[],
  aliases: Readonly<Record<string, string>>,
  rewriting: boolean
): Validation<Record<string, string[]>> {
  const entries = value
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => line.split('=').map((part) => part.trim()));
  if (
    !entries.every(
      (entry): entry is [string, string] =>
        entry.length === 2 && !!entry[0] && !!entry[1]
    )
  ) {
    return {
      ok: false,
      message: 'Use public-model=primary,alternate per line.'
    };
  }
  const pools = entries.map(
    ([publicName, targets]) =>
      [publicName, targets.split(',').map((target) => target.trim())] as const
  );
  if (pools.length && !rewriting) {
    return {
      ok: false,
      message: 'Enable model rewriting to publish model pools.'
    };
  }
  if (
    pools.length > 100 ||
    new Set(pools.map(([publicName]) => publicName)).size !== pools.length ||
    !pools.every(([publicName, targets]) => {
      const primary = models.includes(publicName)
        ? publicName
        : aliases[publicName];
      return (
        !!primary &&
        targets.length >= 2 &&
        targets.length <= 100 &&
        targets[0] === primary &&
        new Set(targets).size === targets.length &&
        targets.every((target) => models.includes(target))
      );
    })
  ) {
    return {
      ok: false,
      message:
        'Use distinct published native targets, starting with the public model’s normal exact target.'
    };
  }
  return { ok: true, value: Object.fromEntries(pools) };
}

export type RouteDraftForm = {
  projectId: string;
  slug: string;
  poolId: string;
  maxBodyBytes: string;
  models: string;
  paidParticipation: boolean;
  paidTargets: PaidTargets;
  paidFallbackTargets: PaidTargets;
  enabled: boolean;
  modelRewriting: boolean;
  aliases: string;
  modelPools: string;
  recoveryAttempts: number;
  chatToCodex: boolean;
  messagesToCodex: boolean;
  codexCLIIdentity: boolean;
  chatToAntigravity: boolean;
  allowanceOrdering: boolean;
  research: boolean;
  researchAccount: string;
  researchServices: readonly ResearchService[];
  researchAccounts: readonly Pick<CodeAccount, 'id'>[];
};

export function buildRouteWrite(
  form: RouteDraftForm
): Validation<CodeRouteWrite> {
  if (!validRouteSlug(form.slug)) {
    return {
      ok: false,
      message:
        'Use a route slug of 1–100 lowercase letters, numbers, dots, underscores or hyphens, starting with a letter or number.'
    };
  }
  if (
    form.maxBodyBytes &&
    (!/^\d+$/.test(form.maxBodyBytes) ||
      Number(form.maxBodyBytes) < 1 ||
      Number(form.maxBodyBytes) > 1073741824)
  ) {
    return {
      ok: false,
      message: 'Use a body limit from 1 to 1073741824 bytes, or leave it empty.'
    };
  }
  if (
    form.research &&
    (!form.researchAccounts.some(
      (account) => account.id === form.researchAccount
    ) ||
      !form.researchServices.length)
  ) {
    return {
      ok: false,
      message: 'Choose an eligible research account and at least one service.'
    };
  }
  const routeModels = parseNativeModels(form.models);
  if (!routeModels.ok) return routeModels;
  const aliasResult = parseAliases(
    form.aliases,
    routeModels.value,
    form.modelRewriting
  );
  if (!aliasResult.ok) return aliasResult;
  const poolResult = parseModelPools(
    form.modelPools,
    routeModels.value,
    aliasResult.value,
    form.modelRewriting
  );
  if (!poolResult.ok) return poolResult;
  const selectedTargets = (values: PaidTargets): PaidTargets =>
    form.paidParticipation
      ? Object.fromEntries(
          routeModels.value.flatMap((model) =>
            values[model]?.provider_id ? [[model, values[model]]] : []
          )
        )
      : {};
  const targets = selectedTargets(form.paidTargets);
  const fallbackTargets = selectedTargets(form.paidFallbackTargets);
  if (
    Object.values({ ...targets, ...fallbackTargets }).some(
      (target) => !target.slot_id
    )
  ) {
    return {
      ok: false,
      message: 'Choose a credential slot for every paid API target.'
    };
  }
  if (Object.keys(fallbackTargets).length && !form.recoveryAttempts) {
    return {
      ok: false,
      message: 'Enable bounded recovery before selecting paid fallback targets.'
    };
  }
  if (Object.keys(fallbackTargets).some((model) => !!targets[model])) {
    return {
      ok: false,
      message:
        'Choose either direct paid selection or subscription-first fallback for each native model.'
    };
  }
  return {
    ok: true,
    value: {
      paid_participation: form.paidParticipation,
      paid_targets: targets,
      paid_fallback_targets: fallbackTargets,
      project_id: form.projectId,
      slug: form.slug,
      pool_id: form.poolId,
      max_body_bytes: form.maxBodyBytes ? Number(form.maxBodyBytes) : null,
      models: routeModels.value,
      aliases: aliasResult.value,
      model_rewriting: form.modelRewriting,
      model_pools: poolResult.value,
      chat_to_codex: form.chatToCodex,
      messages_to_codex: form.messagesToCodex,
      codex_cli_identity: form.codexCLIIdentity,
      chat_to_antigravity: form.chatToAntigravity,
      allowance_ordering: form.allowanceOrdering,
      initial_recovery_attempts: form.recoveryAttempts,
      tools: form.research
        ? {
            account_id: form.researchAccount,
            services: [...form.researchServices]
          }
        : null,
      enabled: form.enabled
    }
  };
}
