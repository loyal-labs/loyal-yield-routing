// Package hastra holds the Hastra vault facts the workers use, each defined
// once: the two program ids, their PDAs and pools, the account order of the
// three conversions a smart account sends (stake deposit, stake redeem, mint
// deposit), and the stake price a conversion is quoted at.
//
// Source: hastra-sol-vault, programs vault-mint and vault-stake (built with
// feature pool-auto for the AUTO pool). Neither program publishes an IDL on
// chain; the account orders here are the source's Accounts structs.
//
//   - Mint program (9WUy…): deposit takes USDC into its pool and mints the
//     same amount of wYLDS, instantly, with no fee. Its way back,
//     request_redeem, is a queue the Hastra admin settles; it is not
//     built here. wYLDS leaves through the stake program instead, and AUTO
//     leaves to USDC through a swap.
//   - Stake program (5uJg…, the AUTO pool): deposit takes wYLDS into its pool
//     and mints AUTO at amount·scale/price; redeem burns AUTO and pays
//     shares·price/scale wYLDS from the pool. Both are instant, with no fee
//     and no unbonding, and both floor (StakePrice). Both refuse with
//     PriceTooStale once the price is older than its max staleness: the
//     price is pushed by Chainlink verify_price, not by us.
//
// Every conversion requires each user token account to be owned by the
// signer (InvalidTokenOwner), and every protocol account is a PDA or the pool
// its config names, which the programs check. A policy pins all of them
// (StakeDepositAllowed); only the amount is free.
package hastra
