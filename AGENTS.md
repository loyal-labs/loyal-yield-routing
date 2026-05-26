<!-- BEGIN:nextjs-agent-rules -->
# This is NOT the Next.js you know

This version has breaking changes. APIs and project structure may differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` before writing any code. Heed deprecation notices.
<!-- END:nextjs-agent-rules -->

## Project Setup

This is a Bun-managed Next.js app. Use `bun install`, `bun dev`, `bun run build`, and `bun run lint`.

## Architecture

This project should follow Loyal's vertical-slice architecture style as it grows.

### Current Structure

The current app surface is intentionally small. `src/app/` owns App Router routes and route handlers. `src/app/page.tsx` is the main page entrypoint, `src/app/layout.tsx` is the root document shell, and `src/app/globals.css` is the global Tailwind/CSS entrypoint.

### Vertical Slice Direction

Organize new work by owning feature. Keep route files as thin orchestration layers and move reusable UI, domain logic, server logic, data access, and integrations into the feature that owns them.

For substantial new features, prefer this shape:

```text
src/features/<feature-name>/
  index.ts        # public entrypoints only
  ui/
  server/
  domain/
  data/
  integrations/
  types.ts
```

Extend an existing slice in place when behavior belongs to that feature. Avoid deep imports across slices; import through a slice's public `index.ts` when cross-slice access is truly needed. Keep feature-specific behavior inside its owning slice until it is clearly reused by multiple slices, then promote stable primitives to `src/lib/`.

Keep `src/app/**/page.tsx`, `layout.tsx`, and route handlers focused on composition, request handling, and wiring. Preserve server/client boundaries. Server-only helpers should live in dedicated server modules such as `server.ts` or `*.server.ts` and must not be imported by client components.

### Shared Library Direction

Use `src/lib/` for cross-slice infrastructure and integration primitives only. Do not create broad horizontal folders for feature-specific code just because files share a technical type.

Examples of code that may belong in `src/lib/` after reuse is proven include framework-safe utilities, shared API clients, validation or serialization helpers, and stable integration wrappers used by multiple features.

## Squads Testing

Use `bun run test:squads` for the lean Rust test crate around Squads smart-account flows. Use `bun run test:squads:e2e` for the heavier ignored historical Kamino replay when changes touch route policy composition, heap/compute assumptions, or replay-sensitive behavior.

The helper crate lives in `crates/squads-test-harness` and should stay focused on reusable LiteSVM setup, Squads PDA derivation, instruction builders, account seeding helpers, and deterministic policy scenarios.

The Rust test crate follows this module layout:

```text
crates/squads-test-harness/src/
  lib.rs                         # public facade and prelude
  squads.rs                      # Squads PDA derivation, settings setup, payload basics
  runtime.rs                     # LiteSVM setup, funded contexts, program loading, tx sending
  policies.rs                    # raw policy-family facade
  policies/
    lifecycle.rs                 # policy removal/settings lifecycle helpers
    spending_limits.rs           # spending-limit policy creation
    program_interaction/
      common.rs                  # shared Squads ProgramInteraction encoding
      stable_swap.rs             # Jupiter and Loyal Hub stable-swap constraints
      kamino.rs                  # Kamino reserve constraints and legacy Kamino route helpers
      route_bundles.rs           # all-in-one route policy builders
  yield_route.rs                 # route-level policy bundles for tests
  protocols.rs                   # mock protocol data, SPL seeding, SBF mock loading
  types.rs                       # shared public harness structs and crate-private wire types
```

Prefer `squads_test_harness::prelude::*` for scenario tests and module-qualified imports when a test is intentionally exercising one slice. Keep route-level orchestration in `yield_route.rs`; keep raw Squads policy instruction builders in `policies/**`; keep mock protocol state and instruction data in `protocols.rs`; keep low-level Squads account and payload primitives in `squads.rs`.

Root-level re-exports exist for compatibility, but new reusable helpers should live in the owning module above instead of becoming broad horizontal utilities. Preserve `pub(crate)` for Squads wire types unless a test truly needs them as public API.

Keep Squads test scaffolding small. Add app-specific test programs or scenarios only when a real yield-routing flow needs them, and prefer deterministic helpers over broad framework wrappers.

## Git and Pull Requests

Use the same commit and PR conventions as Loyal's main repositories.

### Commit Conventions

Commits should use Conventional Commits:

```text
type(scope): description
```

Allowed types: `feat`, `fix`, `chore`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, and `revert`.

Scope is optional but encouraged. Use the area being changed, such as `yield-routing`, `ui`, `docs`, or `ci`.

Commit rules: never add `Co-Authored-By` trailers or co-author attribution, keep the subject line under 100 characters, use imperative mood in the description, do not end the subject line with a period, and validate locally before pushing with the relevant lint/test commands for the files changed.

### Branch and Worktree Conventions

When working from a Linear issue, branches should follow the Linear-style format:

```text
<issue-number-short-description>
```

Example:

```text
ASK-123-add-yield-routing
```

In this smaller project, do not switch branches or create worktrees unless the task calls for it.

### Pull Requests

- PR titles must follow the same conventional commit format: `type(scope): description`.
- PR bodies should be a simple one or two sentence summary of the changes, without heavy templates or checklists unless the repo later adopts one.
- Only merge after required checks and deployment previews are successful.
- Prefer squash-and-merge for PRs.
- Keep PRs scoped to one feature or fix; avoid mixing unrelated refactors with product changes.
