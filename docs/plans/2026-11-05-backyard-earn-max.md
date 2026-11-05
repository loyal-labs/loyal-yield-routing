# Backyard and Earn MAX: state, next step, open items

Status as of 2026-11-05. Partner handoffs are `docs/backyard-looping-partner-handoff.md` and `docs/backyard-rwa-partner-handoff.md`. Deploy commands are in `deploy/hetzner/README.md` ("Release"). Worker design rules are in `docs/workers/facts.md`. The Rust to Go swap finished in October and there is no Rust fallback.

## State

- Earn MAX is the Backyard Voltr RWA vault (route template `rwa-multiply`). Retail, observer and Backyard run as `loyal-engine` and `loyal-observer` on the Hetzner workers host.
- Lane caps are the highest ladder level under each lane's LTV brake (`go/workers/internal/backyard/route_catalog.go`, `maxLevel`). AUTO/PYUSD, AUTO/wYLDS, Maple and Prime cap at 3x and ONyc at 2.5x. The ladder is `{1, 1.5, 1.75, 2, 2.5, 3}` (migration `0114`).
- The vault charges a 40 bps issuance fee, kept in the vault for prior holders, and holds a $5k idle buffer. A fee-terms mismatch latches the route durably. To change the fee, stop the unit, run `deploy backyard <sha> --stopped` (which starts the release), sign, then clear the hold with `loyal-engine backyard clear-hold --vault <name> --reason "<text>"` and wait for the NAV to reconcile.
- NAV is the exit value, not the Kamino mark. The Earn MAX APY badge shows the current rate, stored per Pacific day in `multiply_route_current_apy_days` (migration `0124`).
- `BACKYARD_VOLTR_*` settings no longer exist in the repo, and the dead voltr route is deleted (#321).
- The owner's Oct 26 calls are settled: the fee and buffer shipped, older mobile builds catch up on their own, and there is no wallet-move path and no compensation.

## Next step

Check the wYLDS term lane before its maturity. The Oct 13 note says maturity 2026-11-10 23:23 UTC with a keep-alive check on or after 2026-11-07. The Oct 20 gauge said 2026-11-14 03:09 UTC. The engine does not close the lane at maturity: it keeps the lane alive or delevers to zero debt, then re-levers on a new term.

## Open

- The AUTO/wYLDS overflow lane `6CpLVyoX` was not unwound. It is an active overflow lane of AUTO/PYUSD, about $10.6k collateral and $7.0k debt on 2026-10-25. The engine steps it down below a 1.5% spread, but only while the vault is settled.
- The Squads 200k/day stage allowance needs an owner decision.
- Retail de-layering R2, R3 and R4 from `~/.claude/plans/linus-torvalds-looked-at-expressive-unicorn.md`. R2 is one writer session. R3 is v1 transactions with no lookup tables; `go/workers/internal/fleetexec/lookup_*.go` is still present. R4 is atomic cross-mint. Closing the existing per-vault lookup tables on chain needs a separate owner go.
- The observer watches only policy markets plus five safe markets, so funds in an unwatched market read as empty.
- A dust raw unit keeps a position active while `VaultWithdrawn` says withdrawn.
- The ladder ranks a fee-unknown route as 0, and the fee is counted in borrow proceeds.
- Ranger `allowAnyAdaptor`, the 300 USDC and 0.5 SOL transfers, and one engine process serving both vaults were open on the owner's side on 2026-10-21.
- Lane 3 of the Squads policy speed-up is done in the repo: no `backyard-voltr-authorization` CI job remains. DFlow `swap_with_destination` is only worth doing if it can pin the destination.
