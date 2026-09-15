export type HxtkOperatorMode = "execute" | "reconcile" | null;

export type HxtkCli = Readonly<{
  argv: readonly string[];
  step: string;
  mode: HxtkOperatorMode;
  has: (flag: string) => boolean;
  value: (flag: string) => string | undefined;
}>;

const BOOLEAN_FLAGS = new Set([
  "--simulate",
  "--execute",
  "--reconcile",
  "--allow-repeat",
  "--break-claim",
  "--send",
]);

const VALUE_FLAGS = new Set([
  "--journal",
  "--expect-seed",
  "--seed",
  "--policy-journal",
  "--repair-journal",
  "--cancel-journal",
  "--request-journal",
  "--claim-journal",
]);

function duplicateFlag(flag: string): never {
  throw new Error(`${flag} may be specified only once`);
}

/**
 * Parse the reset CLI arguments. If a complete emitted shell command is passed
 * in, everything through the `reset:hxtk` executable marker is ignored so the
 * same parser can validate recovery commands without a second token parser.
 */
export function parseHxtkCli(argv: readonly string[]): HxtkCli {
  const marker = argv.indexOf("reset:hxtk");
  const normalized = (marker >= 0 ? argv.slice(marker + 1) : argv)
    .filter((value) => value !== "--");
  let step = "";
  const flags = new Set<string>();
  const values = new Map<string, string>();

  for (let index = 0; index < normalized.length; index += 1) {
    const token = normalized[index]!;
    if (token.startsWith("--")) {
      if (BOOLEAN_FLAGS.has(token)) {
        if (flags.has(token)) duplicateFlag(token);
        flags.add(token);
        continue;
      }
      if (VALUE_FLAGS.has(token)) {
        if (values.has(token)) duplicateFlag(token);
        const value = normalized[index + 1];
        if (value === undefined || value.length === 0 || value.startsWith("--")) {
          throw new Error(`${token} requires a value`);
        }
        values.set(token, value);
        index += 1;
        continue;
      }
      throw new Error(`unknown HXtk reset argument ${token}`);
    }
    if (step.length > 0) throw new Error(`unexpected HXtk reset positional argument ${token}`);
    step = token;
  }

  const modes = ["--simulate", "--execute", "--reconcile"]
    .filter((flag) => flags.has(flag));
  if (modes.length > 1) {
    throw new Error(`${modes.join(" and ")} are mutually exclusive`);
  }

  const mode: HxtkOperatorMode = flags.has("--execute")
    ? "execute"
    : flags.has("--reconcile")
      ? "reconcile"
      : null;

  return {
    argv: normalized,
    step,
    mode,
    has: (flag: string) => flags.has(flag) || values.has(flag),
    value: (flag: string) => values.get(flag),
  };
}
