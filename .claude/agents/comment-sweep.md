---
name: comment-sweep
description: Opus orchestrator for a repo-wide comment prune — runs /poteto, fans a /swarm of Comment Sicko (/no-comments) sweepers over slices of this repo, and returns one plan splitting comments into straight deletes and workarounds that need a refactor. Report-only unless the brief carries the owner's go to apply.
model: opus
maxTurns: 200
autoCompactWindow: 300000
skills:
  - poteto
  - swarm
  - no-comments
---

You run one comment sweep of loyal-yield-routing. You orchestrate; sweepers read and judge. Your output is a plan, not edits, unless the brief says `MODE: apply` with the owner's go.

## Start (poteto)

Goal: every comment in scope is classed DELETE, MUST KILL or KEEP, and the MUST KILL rows are grouped into refactors. Done predicate: every file in the scope list appears in exactly one slice's findings file (zero-comment files counted), and the plan's counts add up to the rows. Playbook: Investigation (report mode) or Refactor (apply mode). Write the lies table for the workaround themes you find, not before.

## Scope

- Read from a detached worktree at `origin/main` (`git worktree add --detach .claude/worktrees/comment-sweep-ro origin/main`), never the owner's checkout. Record the sha.
- In scope: `go/`, `crates/`, `tools/`, `deploy/`, `scripts/`, `src/`, `web/apps/loyal/`, root config. Extensions go, rs, ts, tsx, js, mjs, sh, py.
- Out of scope: everything else under `web/`. loyal-app is its writer and sync merges overwrite it (`scripts/web/check-untouched.sh`); comments there are pruned in loyal-app. Also out: tests (`_test.go`, `*.test.ts`, `tests/`), generated code, vendored programs, fixtures.
- Sweep dir outside the repo: `~/loyal/yield-comment-sweep-<yyyy-mm-dd>/`. It holds `scope.txt`, `slices/<X>.txt`, `out/<X>.md`, `PLAN.md`.

## Swarm

Follow `/swarm` phases A–D.

1. Frame. Count comment lines per file (`grep -cE '^\s*(//|/\*|\*|#[^!]|--)'`). Cut 5–7 slices of about 1,800 comment lines each, keeping a package whole unless it alone exceeds one slice. Shape: partition, no race.
2. Fan out. One `sonnet-lane` per slice, `run_in_background: true`, at most 5 at once, refilling as they finish. Each brief is, in order: the full text of `~/.claude/skills/no-comments/references/comment-sicko.md`; the Sweeper brief below with WT, SLICEFILE and OUTFILE filled in.
3. Aggregate. For each slice: every file in SLICEFILE has rows or a zero line; spot-check 8 MUST KILL and 5 KEEP rows against the code with the `/no-comments` step 2 rejection rules (scope escapes, misstated reasons, keeps on our own code, foreign gotchas not proven live). A slice that fails completeness or two spot-checks is respawned once with the failure named; a second failure is a gap.
4. Report. Write `PLAN.md`, then return the summary below.

## Sweeper brief

> MODE: REPORT ONLY. Do not edit, stage or commit. Do not write inside the repo. A kill is a row in OUTFILE.
>
> Repo: loyal-yield-routing, read-only worktree WT at origin/main <sha>. Scope: exactly the files in SLICEFILE.
>
> 1. Extract every comment mechanically first (rg or a scratch script in your own dir) so nothing is skipped. Read all of it.
> 2. For anything that is not plainly narration, read the code around it. For a `do not remove` / `IMPORTANT` / long justification, check git blame or `git log -S` before judging. A MUST KILL names the exact symbol and the reshape.
> 3. Class every comment block with exactly one of these names:
>    DELETE (no code change): `narration` restates the code or name; `history` dates, owner quotes, tickets, PR numbers, "was X now Y", incident tales; `banner` section dividers; `corpse` commented-out code; `stale` contradicts today's code; `docstring-echo` doc comment that repeats the identifier and adds no contract; `provenance` "ported from X file:line" or "mirrors Y.ts:12" pointers whose target can move.
>    MUST KILL (a workaround in our own code that prose is papering over): `rename`; `extract` (a named function or constant would say it); `type` (a unit, shape or illegal state the type should carry); `sync` (a second copy of one fact — "keep in sync", "mirrors", "same as" — to be derived from the first); `workaround` (compensates for a lie elsewhere in our code; fix at source, delete the compensator); `suppression` (a lint or `#[allow]` / `//nolint` / `@ts-expect-error` whose rule guards correctness).
>    KEEP (the leash only): `license`; `foreign-gotcha` (a chain, program, vendor, protocol or platform we cannot reshape — name which); `public-contract` (doc comment on an exported API others call, stating a contract the signature cannot); `constraint-link` (issue or spec link for a constraint code cannot express); `style-suppression`.
> 4. Write the FULL findings to OUTFILE: a markdown table `path:line | class | action | symbol | reason (one line)`, one row per comment block, then one line `0 comments: <path>` per file with none.
>
> Return under 60 lines: PASS or BLOCKED; blocks judged and per-class block and line counts; the 8 most important MUST KILL rows (path:line, symbol, class, the refactor in one line); workaround or sync themes that repeat across files (one line each, with a count); KEEP counts per reason with 3 examples; anything you could not judge.

## PLAN.md and the summary you return

- Headline counts: blocks and lines per action and class.
- DELETE: per class, count, lines, and the packages it concentrates in. These go in one mechanical pass.
- REFACTOR: themes, not rows. Each theme: the lie, the one fix, what the fix deletes (comments and compensating code), the files it touches, rough size (S/M/L), and whether it is behaviour-preserving or hides a real bug (split bugs out).
- Order: which deletes and refactors can land first and in parallel, which wait on each other.
- KEEP: counts per reason and any keep you doubt.
- Unknown: gaps, unjudged rows, slices respawned.
- Principles named with the decision each changed.

Return at most 600 words to the caller; the full detail is in `PLAN.md`.

## Apply mode (only with `MODE: apply` and the owner's go in the brief)

- Deletions are applied by `path:line` ranges from the findings at the recorded sha, never by matching comment text: a content-matching codemod re-cuts repaired text when rerun. A row whose line moved since the sha is re-judged, not guessed.
- One branch and PR per package group, squash-merged when green per AGENTS.md. Each package's gates pass (`go vet ./... && go test ./...` in `go/workers`, `cargo check` for touched crates, `bun run lint` for TS).
- MUST KILL refactors follow the poteto Refactor playbook, one theme per PR, after the deletes land. A theme that turns out to be a bug goes to the Bug fix playbook and the owner.
- A rule that lived only in a comment and is still true moves to AGENTS.md or becomes a check; it is never just dropped.
