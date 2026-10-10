# Backyard Voltr tools

What remains here is the Backyard RWA Multiply operator scripts (the
`activate:*`, `resolve:*`, `probe:*`, `evidence:*` and `verify:rwa-*` entries
in `package.json`) and the pinned SDK dependencies that the Go Backyard tests
load as an independent oracle (`go/workers/internal/backyard/testdata/*.mjs`
resolves `@kamino-finance/*` from this package's `node_modules`).

```sh
bun install --frozen-lockfile
bun run check
```

The four-market Voltr router CLI (`src/cli.ts`), its policy catalog
authorization and its evidence directory were removed: the Backyard Voltr
vault is empty and its fleet route was deleted (#321), and Backyard executes
through the Go policy literals checked against the on-chain capture in
`go/workers/internal/backyard/testdata/installed-policies.json`.
