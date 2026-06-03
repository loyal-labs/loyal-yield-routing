# Jupiter Swap Batch Capacity

This note records the local LiteSVM capacity probe for packing mock Jupiter swaps
into one Solana transaction. The probe lives in
`crates/squads-test-harness/tests/jupiter_swap_batch_size.rs` and is ignored by
default because it is a packet-capacity measurement rather than normal CI
coverage.

Interactive transaction map: [`jupiter-swap-tx-breakdown.html`](./jupiter-swap-tx-breakdown.html).

Run it with:

```bash
cargo test -p squads-test-harness --test jupiter_swap_batch_size -- --ignored --nocapture --test-threads=1
```

## Results

The probe uses the repo's local mock Jupiter stable exact-in instruction:

- Program: `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4`
- Input mint: `USDC_MINT`
- Output mint: `PYUSD_MINT`
- Per-swap data: 90 bytes
- Per-swap token movement: SPL Token `transfer_checked` from user input to mock
  Jupiter input reserve, then signed `transfer_checked` from mock Jupiter output
  reserve to user output.
- Packet limit used by the probe: 1,232 serialized bytes.

Measured maximums:

| Transaction shape | Max swaps | Failing next size | Notes |
| --- | ---: | ---: | --- |
| Legacy, independent user signer per swap | 3 | 4 swaps = 1,310 bytes | No address lookup table. |
| v0 + ALT, independent user signer per swap | 5 | 6 swaps = 1,312 bytes | ALT removes non-signer account keys from the static message, but every user signature remains. |
| v0 + ALT, single batch signer | 9 | 10 swaps = 1,248 bytes | Upper bound for a delegated/single-owner model, not independent user custody. |
| v0 + ALT, compact batch CPI program, single batch signer | 20 | 21 swaps = 711 bytes, but `MaxInstructionTraceLengthExceeded` | One outer batch instruction loops CPI calls into mock Jupiter. This is runtime-trace limited before packet-size limited. |

For the independent multi-user target, v0 transactions plus an address lookup
table raise the local mock capacity from 3 swaps to 5 swaps. The remaining
bottleneck is the serialized transaction packet size, mainly signatures plus
per-instruction data. Signer accounts cannot be loaded through an ALT.

Real Jupiter routes can be larger than this mock. They often include more
accounts, setup/cleanup instructions, compute budget instructions, and route
data, so these numbers are optimistic for real API-generated swaps.

## Proposed Compact Batch Setup

The desired high-density setup is:

1. Users pre-delegate authority to one batch signer, or the product uses a
   legitimate custody/escrow/batch-authority model.
2. A v0 transaction uses one ALT containing all reusable and per-user token
   accounts.
3. The transaction includes a compute-budget instruction and one batch-program
   instruction.
4. The batch program loops over compact swap records and CPIs into Jupiter for
   each swap.
5. Per-swap transaction data carries only:
   - direction: `0 = USDC -> PYUSD`, `1 = PYUSD -> USDC`
   - input amount
   - expected output amount

The test-only batch program lives in `mock-yield-protocols-program` and is loaded
in LiteSVM at a separate test program id. It executes actual CPIs into the mock
Jupiter program id, and each mock Jupiter swap does two SPL Token CPIs.

The outer batch instruction account layout is:

| Position | Account | Placement |
| ---: | --- | --- |
| 0 | Batch signer / fee payer | Static signer |
| 1 | Jupiter v6 program id | ALT read-only |
| 2 | SPL Token program id | ALT read-only |
| 3 | `USDC_MINT` | ALT read-only |
| 4 | `PYUSD_MINT` | ALT read-only |
| 5 | Mock Jupiter USDC reserve token account | ALT writable |
| 6 | Mock Jupiter PYUSD reserve token account | ALT writable |
| 7 | Mock Jupiter swap authority PDA | ALT read-only |
| 8.. | Two temporary token accounts per swap: USDC account, PYUSD account | ALT writable |

The compact batch instruction data is:

| Bytes | Field |
| ---: | --- |
| 8 | Batch discriminator |
| 1 | Swap count |
| `17 * n` | Swap records |

Each 17-byte swap record is:

| Bytes | Field |
| ---: | --- |
| 1 | Direction |
| 8 | Input amount |
| 8 | Output amount |

## Compact Batch 20-Swap Breakdown

At the measured max of 20 swaps, the transaction is:

- Serialized size: 690 bytes.
- Top-level instructions: 2.
- Static account keys: 3.
- ALT-loaded accounts: 47.
- Total executable/loaded account keys: 50.
- Signatures: 1.
- Address lookup table descriptors: 1.
- Successful runtime trace length: `2 + (20 * 3) = 62`.
- Failing next runtime trace length: `2 + (21 * 3) = 65`, above the runtime max instruction trace length of 64.

The three static account keys are:

| Static key | Why static |
| --- | --- |
| Batch signer / fee payer | Required transaction signature. |
| Compute Budget program id | Invoked by the top-level compute-budget instruction. Invoked program ids stay static. |
| Batch program id | Invoked by the top-level batch instruction. Invoked program ids stay static. |

The 47 ALT-loaded accounts are:

| Count | Account group | Access |
| ---: | --- | --- |
| 40 | Per-swap temporary token accounts: 20 USDC accounts and 20 PYUSD accounts | Writable |
| 2 | Shared mock Jupiter reserve token accounts: USDC reserve and PYUSD reserve | Writable |
| 2 | Shared mint accounts: `USDC_MINT` and `PYUSD_MINT` | Read-only |
| 1 | Jupiter v6 program id, passed as an account for CPI | Read-only |
| 1 | SPL Token program id, passed through to mock Jupiter | Read-only |
| 1 | Mock Jupiter swap authority PDA | Read-only |

The serialized transaction byte budget is:

| Component | Bytes | Notes |
| --- | ---: | --- |
| Signatures | 65 | One shortvec length byte plus one 64-byte signature. |
| Message version and header | 4 | v0 prefix plus three-byte message header. |
| Static account keys | 97 | One shortvec length byte plus three 32-byte keys. |
| Recent blockhash | 32 | Standard transaction blockhash. |
| Top-level instruction section | 410 | One compute-budget instruction plus one compact batch instruction. |
| ALT lookup section | 82 | One lookup table descriptor, 42 writable indexes, and 5 read-only indexes. |
| **Total** | **690** | Leaves 542 bytes of packet headroom, but runtime trace is already the limiter. |

Inside the top-level instruction section:

| Component | Bytes |
| --- | ---: |
| Instruction vector length | 1 |
| Compute-budget instruction | 8 |
| Batch instruction program/account index bytes | 50 |
| Batch instruction data length prefix | 2 |
| Batch instruction data | 349 |

The largest single part is the batch instruction data, but packet size is not the
binding limit for this design. The binding limit is the instruction trace:

```text
compute budget ix        = 1 trace entry
batch ix                 = 1 trace entry
per swap Jupiter CPI     = 1 trace entry
per swap SPL input CPI   = 1 trace entry
per swap SPL output CPI  = 1 trace entry
```

That gives `2 + 3n` trace entries. With the runtime limit at 64, `n = 20` is the
largest value that executes in this mock. The `n = 21` transaction is still only
711 bytes, but fails with `InstructionError(1, MaxInstructionTraceLengthExceeded)`.

If the batch program did direct SPL transfers instead of CPIing into Jupiter,
the trace model would be closer to `2 + 2n`, allowing about 31 swaps before the
same trace limit. That would no longer test the intended Jupiter-CPI setup.

## Serialized 20-Swap Transaction Dump

The deterministic dump below is produced by:

```bash
cargo test -p squads-test-harness --test jupiter_swap_batch_size dumps_serialized_v0_alt_compact_batch_transaction_layout -- --ignored --nocapture --test-threads=1
```

The test builds the 20-swap compact batch transaction, dumps its serialized
bytes, and then sends the same transaction through LiteSVM.

```text
serialized_transaction_len=690
swap_count=20

0000: 01 4a 89 08 01 50 90 d4 ed 5f 55 6b db fc 5a 38
0010: e4 af 35 3e e8 7d aa 12 8c 17 0e 75 6b e5 fa 46
0020: b6 e0 7c dc 5c 5d c8 be 48 4f 79 4a 28 c5 be 4b
0030: b6 cf fb ea 11 19 7b a8 ac d8 65 e9 3b 88 58 8d
0040: 0b 80 01 00 02 03 14 c7 0c 7e 0c 4c 77 12 75 6e
0050: bb df d3 33 17 be 8f df 76 35 88 24 e6 36 09 89
0060: 12 ce d8 1c 1f b1 03 06 46 6f e5 21 17 32 ff ec
0070: ad ba 72 c3 9b e7 bc 8c e5 bb c5 f7 12 6b 2c 43
0080: 9b 3a 40 00 00 00 42 42 42 42 42 42 42 42 42 42
0090: 42 42 42 42 42 42 42 42 42 42 42 42 42 42 42 42
00a0: 42 42 42 42 42 42 32 a7 2a 80 43 f4 ce 5c 96 29
00b0: 94 ae 13 7d 35 0a 0b ff 30 32 e9 71 9e 29 23 63
00c0: 8c bc 6a bf cc 9f 02 01 00 05 02 c0 5c 15 00 02
00d0: 30 00 2d 2e 31 2f 04 03 30 05 19 06 1a 07 1b 08
00e0: 1c 09 1d 0a 1e 0b 1f 0c 20 0d 21 0e 22 0f 23 10
00f0: 24 11 25 12 26 13 27 14 28 15 29 16 2a 17 2b 18
0100: 2c dd 02 04 00 00 00 00 00 00 00 14 00 e8 03 00
0110: 00 00 00 00 00 e8 03 00 00 00 00 00 00 01 e8 03
0120: 00 00 00 00 00 00 e8 03 00 00 00 00 00 00 00 e8
0130: 03 00 00 00 00 00 00 e8 03 00 00 00 00 00 00 01
0140: e8 03 00 00 00 00 00 00 e8 03 00 00 00 00 00 00
0150: 00 e8 03 00 00 00 00 00 00 e8 03 00 00 00 00 00
0160: 00 01 e8 03 00 00 00 00 00 00 e8 03 00 00 00 00
0170: 00 00 00 e8 03 00 00 00 00 00 00 e8 03 00 00 00
0180: 00 00 00 01 e8 03 00 00 00 00 00 00 e8 03 00 00
0190: 00 00 00 00 00 e8 03 00 00 00 00 00 00 e8 03 00
01a0: 00 00 00 00 00 01 e8 03 00 00 00 00 00 00 e8 03
01b0: 00 00 00 00 00 00 00 e8 03 00 00 00 00 00 00 e8
01c0: 03 00 00 00 00 00 00 01 e8 03 00 00 00 00 00 00
01d0: e8 03 00 00 00 00 00 00 00 e8 03 00 00 00 00 00
01e0: 00 e8 03 00 00 00 00 00 00 01 e8 03 00 00 00 00
01f0: 00 00 e8 03 00 00 00 00 00 00 00 e8 03 00 00 00
0200: 00 00 00 e8 03 00 00 00 00 00 00 01 e8 03 00 00
0210: 00 00 00 00 e8 03 00 00 00 00 00 00 00 e8 03 00
0220: 00 00 00 00 00 e8 03 00 00 00 00 00 00 01 e8 03
0230: 00 00 00 00 00 00 e8 03 00 00 00 00 00 00 00 e8
0240: 03 00 00 00 00 00 00 e8 03 00 00 00 00 00 00 01
0250: e8 03 00 00 00 00 00 00 e8 03 00 00 00 00 00 00
0260: 01 90 90 90 90 90 90 90 90 90 90 90 90 90 90 90
0270: 90 90 90 90 90 90 90 90 90 90 90 90 90 90 90 00
0280: 00 2a 05 04 07 09 0b 0d 0f 11 13 15 17 19 1b 1d
0290: 1f 21 23 25 27 29 2b 2d 08 0a 0c 0e 10 12 14 16
02a0: 18 1a 1c 1e 20 22 24 26 28 2a 2c 2e 05 00 01 03
02b0: 06 02
```

The corresponding byte chunks are:

| Offset range | Bytes | Meaning |
| --- | ---: | --- |
| `0000..0000` | 1 | Signature count shortvec: `01`. |
| `0001..0040` | 64 | Signature 0 from the batch signer. |
| `0041..0041` | 1 | Message version prefix: `80`, meaning v0. |
| `0042..0044` | 3 | Message header: `01 00 02` = one required signature, zero readonly signed accounts, two readonly unsigned static accounts. |
| `0045..0045` | 1 | Static account key count shortvec: `03`. |
| `0046..0065` | 32 | Static account 0: batch signer / fee payer. |
| `0066..0085` | 32 | Static account 1: Compute Budget program id. |
| `0086..00a5` | 32 | Static account 2: batch program id. |
| `00a6..00c5` | 32 | Recent blockhash. |
| `00c6..00c6` | 1 | Compiled instruction count shortvec: `02`. |
| `00c7..00ce` | 8 | Compute-budget instruction: program index `01`, zero account indexes, five data bytes `02 c0 5c 15 00` for `SetComputeUnitLimit(1_400_000)`. |
| `00cf..00cf` | 1 | Batch instruction program id index: `02`. |
| `00d0..00d0` | 1 | Batch instruction account index count shortvec: `30` = 48 accounts. |
| `00d1..0100` | 48 | Batch instruction account indexes. See decoded account-index map below. |
| `0101..0102` | 2 | Batch instruction data length shortvec: `dd 02` = 349 bytes. |
| `0103..010a` | 8 | Batch discriminator: `04 00 00 00 00 00 00 00`. |
| `010b..010b` | 1 | Swap count: `14` = 20. |
| `010c..025f` | 340 | Twenty compact swap records, 17 bytes each. Record `i` starts at `0x010c + i * 0x11`: one direction byte, eight little-endian input-amount bytes, eight little-endian output-amount bytes. |
| `0260..0260` | 1 | Address lookup table count shortvec: `01`. |
| `0261..0280` | 32 | Lookup table account key. |
| `0281..0281` | 1 | Writable lookup index count shortvec: `2a` = 42. |
| `0282..02ab` | 42 | Writable ALT indexes: reserves plus 40 user token accounts. |
| `02ac..02ac` | 1 | Readonly lookup index count shortvec: `05`. |
| `02ad..02b1` | 5 | Readonly ALT indexes: Jupiter, SPL Token, mints, and authority. |

The 48 batch instruction account-index bytes are:

```text
00 2d 2e 31 2f 04 03 30 05 19 06 1a 07 1b 08 1c
09 1d 0a 1e 0b 1f 0c 20 0d 21 0e 22 0f 23 10 24
11 25 12 26 13 27 14 28 15 29 16 2a 17 2b 18 2c
```

Decoded:

| Batch account position | Compiled account index | Meaning |
| ---: | ---: | --- |
| 0 | `0x00` | Static batch signer / fee payer. |
| 1 | `0x2d` | ALT-loaded Jupiter program id. |
| 2 | `0x2e` | ALT-loaded SPL Token program id. |
| 3 | `0x31` | ALT-loaded USDC mint. |
| 4 | `0x2f` | ALT-loaded PYUSD mint. |
| 5 | `0x04` | ALT-loaded USDC reserve. |
| 6 | `0x03` | ALT-loaded PYUSD reserve. |
| 7 | `0x30` | ALT-loaded mock Jupiter authority. |
| `8 + 2i` | `0x05 + i` | Swap `i` USDC token account, for `i = 0..19`. |
| `9 + 2i` | `0x19 + i` | Swap `i` PYUSD token account, for `i = 0..19`. |

The ALT lookup section stores lookup-table indexes, not pubkeys:

```text
writable indexes:
05 04 07 09 0b 0d 0f 11 13 15 17 19 1b 1d 1f 21
23 25 27 29 2b 2d 08 0a 0c 0e 10 12 14 16 18 1a
1c 1e 20 22 24 26 28 2a 2c 2e

readonly indexes:
00 01 03 06 02
```

The v0 compiler loads writable ALT accounts first, then readonly ALT accounts,
after the three static keys. That is why the batch instruction uses account
indexes such as `0x2d` for Jupiter and `0x31` for the USDC mint instead of the
raw ALT indexes `0x00` and `0x02`.

## Single-Signer v0 + ALT Breakdown

The 9-swap upper-bound probe simulates temporary token accounts owned by one
batch authority. It is useful for measuring packet pressure when signatures are
not the bottleneck.

At 9 swaps the transaction is:

- Serialized size: 1,144 bytes.
- Instructions: 9 mock Jupiter stable exact-in instructions.
- Static account keys: 2.
- ALT-loaded accounts: 24.
- Total executable/loaded account keys: 26.
- Signatures: 1.
- Address lookup table descriptors: 1.

The static account keys are:

| Static key | Role |
| --- | --- |
| Batch signer / fee payer | Required signature, writable fee payer, owner of every temporary source and destination token account. |
| Jupiter v6 program id | Invoked program id. The v0 compiler keeps invoked program ids static; they are not loaded from ALT. |

The ALT-loaded accounts are:

| Count | Account group | Access |
| ---: | --- | --- |
| 18 | Per-swap temporary token accounts: 9 USDC input accounts and 9 PYUSD output accounts | Writable |
| 2 | Shared mock Jupiter reserve token accounts: USDC reserve and PYUSD reserve | Writable |
| 2 | Shared mint accounts: `USDC_MINT` and `PYUSD_MINT` | Read-only |
| 1 | SPL Token program id | Read-only |
| 1 | Mock Jupiter swap authority PDA | Read-only |

The test lookup table is seeded with 25 addresses:

1. Jupiter v6 program id.
2. `USDC_MINT`.
3. `PYUSD_MINT`.
4. SPL Token program id.
5. Mock Jupiter USDC reserve token account.
6. Mock Jupiter PYUSD reserve token account.
7. Mock Jupiter swap authority PDA.
8. Two temporary token accounts for each swap.

Only 24 of those are actually loaded through the ALT in the 9-swap transaction.
The Jupiter program id remains static because invoked program ids are excluded
from lookup extraction.

Each of the 9 swap instructions uses this account shape:

| Account position | Account | Access |
| ---: | --- | --- |
| 0 | Batch signer | Signer, read-only in the instruction; writable at the transaction level because it is the fee payer. |
| 1 | This swap's temporary USDC input token account | Writable |
| 2 | This swap's temporary PYUSD output token account | Writable |
| 3 | `USDC_MINT` | Read-only |
| 4 | `PYUSD_MINT` | Read-only |
| 5 | SPL Token program id | Read-only |
| 6 | Mock Jupiter USDC reserve token account | Writable |
| 7 | Mock Jupiter PYUSD reserve token account | Writable |
| 8 | Mock Jupiter swap authority PDA | Read-only |

Each instruction carries 90 bytes of data:

| Bytes | Field |
| ---: | --- |
| 8 | Mock Jupiter stable exact-in discriminator |
| 8 | Input amount |
| 8 | Output amount |
| 2 | Slippage bps |
| 32 | Input mint |
| 32 | Output mint |

The single-signer result does not preserve the independent-user authorization
model. It only applies if the product has a legitimate batch signer, delegated
authority, or custody model that can own or spend from all temporary token
accounts used in the transaction.
