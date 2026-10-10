// Package dflow holds what we know about the DFlow aggregator, so the next
// lane starts here rather than in another repository's test comments. No
// worker sends DFlow swaps: its swap cannot be bounded by a Squads policy, so
// swaps go through Jupiter's shared_accounts_route_v2 (package jupiter).
//
// The program is DF1ow4tspfHX9JwWJsAb9epbkA8hmpSEAtxXy1V27QBH (DFlow
// Aggregator v4); its Anchor IDL (swap_orchestrator, zlib JSON) is at
// Cp2dCjxCWdktak2JiSrh87X6sz31EnDVKoTGtsHJvhYq. The quote API is
// https://quote-api.dflow.net, which answers 403 without the key in the
// environment variable DFLOW_API_KEY (the yield 1Password environment holds
// it); https://dev-quote-api.dflow.net answers without a key.
//
// Why it cannot be bounded (mainnet simulation, 2026-10-10):
//   - The API returns swap (discriminator f8c69e91e17587c8) whenever the
//     output goes to the user's own token account, however the destination is
//     passed. Its fixed accounts are only the token, ATA and system programs,
//     the user, the event authority and the program; the source and
//     destination token accounts and the mints sit among the venue's accounts,
//     at an index that depends on the venue, and the program does not tie the
//     destination to the user. Replacing the user's output account in an
//     AlphaQ route with another wallet's sent the whole output there.
//   - Its amounts (quoted_out_amount u64, slippage_bps u16, platform_fee_bps
//     u16) follow the variable-length actions vector, so no fixed offset holds
//     them.
//   - swap_with_destination (a8ac184dc59c8765) fixes the destination at
//     index 4, but the API emits it only for a foreign destination. DFlow
//     becomes usable if it emits that instruction for the user's own account
//     and confirms the program delivers all output there.
package dflow
