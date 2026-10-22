import { readFileSync } from "node:fs";

const REPO = "loyal-labs/loyal-app";
const SHORT = 8;
const COMPARE_FILE_CAP = 300;
const FILES_PAGE = 100;
const MAX_PAGES = 40;

const PREFIXES = ["apps/mobile/", "packages/smart-account-vaults/"];
const EXACT = new Set([
  "apps/web/src/features/earn-max/voltr/program.ts",
  "apps/web/src/features/earn-max/voltr/plans.ts",
  "apps/web/src/features/earn-max/voltr/visibility.ts",
  "apps/web/src/features/earn-max/voltr/program.test.ts",
  "apps/web/src/lib/smart-accounts/prepared-operation-wire.shared.ts",
  "apps/web/src/lib/smart-accounts/prepared-operation-wire.shared.test.ts",
]);

type ApiFile = { filename: string; previous_filename?: string };
type ApiCommit = {
  sha: string;
  commit: { message: string; committer: { date: string } };
  files?: ApiFile[];
};
type Compare = {
  status: string;
  ahead_by: number;
  commits: ApiCommit[];
  files?: ApiFile[];
};

const pin = readFileSync(
  new URL("../../web/LOYAL_APP_BASE", import.meta.url),
  "utf8",
).trim();
if (!/^[0-9a-f]{40}$/.test(pin)) {
  throw new Error(`web/LOYAL_APP_BASE must hold a full 40-char sha, got "${pin}"`);
}

const token = process.env.GH_TOKEN ?? process.env.GITHUB_TOKEN;
const headers: Record<string, string> = {
  accept: "application/vnd.github+json",
  "x-github-api-version": "2022-11-28",
  ...(token ? { authorization: `Bearer ${token}` } : {}),
};

async function api<T>(path: string): Promise<T> {
  const res = await fetch(`https://api.github.com/repos/${REPO}/${path}`, {
    headers,
  });
  if (!res.ok) {
    throw new Error(
      `GitHub ${res.status} for ${path}: ${(await res.text()).slice(0, 200)}`,
    );
  }
  return (await res.json()) as T;
}

const watched = (name: string) =>
  EXACT.has(name) || PREFIXES.some((p) => name.startsWith(p));
const matching = (files: ApiFile[]) => [
  ...new Set(
    files
      .flatMap((f) => [f.filename, f.previous_filename ?? ""])
      .filter((n) => n !== "" && watched(n)),
  ),
];

const short = (sha: string) => sha.slice(0, SHORT);
const pt = (iso: string) =>
  new Intl.DateTimeFormat("en-CA", {
    timeZone: "America/Los_Angeles",
    dateStyle: "short",
    timeStyle: "short",
    hourCycle: "h23",
  }).format(new Date(iso)) + " PT";

async function compareAll(): Promise<{
  commits: ApiCommit[];
  files: ApiFile[];
  status: string;
}> {
  const commits: ApiCommit[] = [];
  let first: Compare | undefined;
  for (let page = 1; page <= MAX_PAGES; page++) {
    const c = await api<Compare>(
      `compare/${pin}...main?per_page=100&page=${page}`,
    );
    first ??= c;
    commits.push(...c.commits);
    if (c.commits.length === 0 || commits.length >= first.ahead_by) break;
  }
  if (!first) throw new Error("compare returned nothing");
  if (commits.length < first.ahead_by) {
    throw new Error(
      `compare listed ${commits.length} of ${first.ahead_by} commits after ${MAX_PAGES} pages`,
    );
  }
  return { commits, files: first.files ?? [], status: first.status };
}

async function commitFiles(sha: string): Promise<ApiFile[]> {
  const files: ApiFile[] = [];
  for (let page = 1; page <= MAX_PAGES; page++) {
    const c = await api<ApiCommit>(
      `commits/${sha}?per_page=${FILES_PAGE}&page=${page}`,
    );
    const got = c.files ?? [];
    files.push(...got);
    if (got.length < FILES_PAGE) break;
  }
  return files;
}

const { commits, files, status } = await compareAll();
const head = commits.at(-1)?.sha ?? pin;

if (commits.length === 0) {
  console.log(`OK loyal-app main is at the pin ${short(pin)} (${status})`);
  process.exit(0);
}

const truncated = files.length >= COMPARE_FILE_CAP;
if (!truncated && matching(files).length === 0) {
  console.log(
    `OK loyal-app ${short(pin)}..${short(head)}: ${commits.length} new commits, none touch the mobile or moved package paths`,
  );
  process.exit(0);
}

const hits: { c: ApiCommit; names: string[] }[] = [];
for (const c of commits) {
  const names = matching(await commitFiles(c.sha));
  if (names.length > 0) hits.push({ c, names });
}

if (hits.length === 0) {
  console.log(
    `OK loyal-app ${short(pin)}..${short(head)}: ${commits.length} new commits, none touch the mobile or moved package paths`,
  );
  process.exit(0);
}

for (const { c, names } of hits) {
  const subject = c.commit.message.split("\n")[0];
  console.log(`${short(c.sha)}  ${pt(c.commit.committer.date)}  ${subject}`);
  for (const n of names) console.log(`    ${n}`);
}
console.error(
  `port ${short(pin)}..${short(head)} into web/ and bump the pin (web/LOYAL_APP_BASE)`,
);
process.exit(1);
