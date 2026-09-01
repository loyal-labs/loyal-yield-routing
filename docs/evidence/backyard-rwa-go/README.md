# Backyard RWA Go evidence

This directory holds immutable evidence only after a read or signed simulation
has actually occurred. Do not hand-edit `addressesResolved`,
`swapHeadersResolved`, packet sizes, simulation results, or policy hashes.

## Policy resolver

Run the finalized read-only resolver with:

```sh
bun run --cwd tools/backyard-voltr resolve:rwa-multiply-policy-catalog
```

It emits `loyal-backyard-rwa-policy-resolution/v1`, pins the catalog and route
hashes, decodes every candidate market/reserve/mint at a finalized boundary,
derives the eleven tag-1/id-0 Multiply obligations and every Squads vault ATA,
and reports ATA drift or absence. It deliberately returns exit code 2 and
`BLOCKED_SWAP_HEADERS` until all 52 current Jupiter `SharedAccountsRoute`
headers prove the exact authority, source/destination custody, mint,
token-program, role, discriminator, slippage, account-count, and
no-extra-instruction boundary.

## Resolved compiler input

Only a reconciled resolver/quote builder may write
`policy-compiler-input-v1.json`. Its top-level contract is:

```json
{
  "schema": "loyal-backyard-rwa-policy-compiler-input/v1",
  "addressesResolved": true,
  "swapHeadersResolved": true,
  "catalogSha256": "<64 lowercase hex>",
  "resolutionSha256": "<64 lowercase hex>",
  "settings": "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6",
  "authority": "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
  "delegatedSigner": "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
  "accountIndex": 0,
  "policySeedBefore": "56",
  "policies": ["exact 11 lane rows followed by exact 3 split-swap rows"]
}
```

Each policy row carries `name`, `semanticEdgeCount`, and `constraints`. A
constraint carries `programId`, a non-empty unique-index `accountPubkeys`
array, and one or more data predicates. Supported data predicate kinds are
`slice-equals` (`offset`, `valueHex`), `u8-equals`, `u16-equals`,
`u16-less-than-or-equal`, and `u32-equals` (`offset`, `value`).

The Rust compiler accepts exactly this order:

1. eleven `lane/<market>/<collateral>/<debt>` policies in catalog order, each
   with four operation constraints and `semanticEdgeCount: 4`;
2. `swap/stable-to-rwa` with 20 edges;
3. `swap/rwa-to-stable` with 20 edges; and
4. `swap/stable-to-stable` with 12 edges.

It derives seeds 57 through 70 and their policy PDAs, emits deployed-ABI
PolicyCreate and PolicyUpdate instructions, measures full packets with the
actual signature-slot count, and fails if any create or update exceeds 1,232
bytes.

## Installer

After the input exists, the default command is signed-unsent:

```sh
bun run --cwd tools/backyard-voltr activate:rwa-multiply-catalog-policies \
  --input docs/evidence/backyard-rwa-go/policy-compiler-input-v1.json
```

The installer reads finalized Settings and all 14 policy accounts in one
batch. Exact policy bytes are a no-op. An absent policy may be created only at
the next finalized Settings seed. An inexact existing policy may be updated in
place only after its owner, Settings, seed, signer, threshold, timelock, and
hookless ProgramInteraction boundary match. It measures and signed-simulates
before any optional send, persists the exact wire before send, and reconciles
the finalized account bytes afterward.

## Custody and Jupiter prerequisites

`activate:rwa-multiply-custody-atas` derives the nine unique Squads ATAs
shared by all eleven lanes and reads them at finalized commitment. It fails on
any occupied-but-inexact account. Its default is signed simulation only;
`--execute --journal PATH` persists one exact signed packet before send, and
`--reconcile --journal PATH` requires finalized status plus exact post-readback
before a subsequent packet may be attempted.

`resolve:rwa-multiply-jupiter-headers --out PATH` retrieves all 52 directed
catalog edges with bounded concurrency four and persists exact instruction
headers without broadcasting. It fails closed for extra setup or cleanup
instructions, unsupported discriminators, an unprovable token-program
boundary, wrong custody/mint/signer roles, excessive slippage, or unresolved
lookup tables.
