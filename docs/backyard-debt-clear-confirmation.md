# Backyard full-debt repayment confirmation

Ordinary exits that clear all existing borrowing require explicit operator
confirmation at every utilization level. Borrowing capacity can disappear
after repayment.

Automatic liquidation-risk protection remains enabled. It uses the existing
hard-LTV threshold, `min(6000, liquidationThresholdBPS - 1500)`, and verified
fresh position and reserve data. This change does not raise borrowing limits,
change installed policies or enable the separate ONyc flash-exit proof.

## Actions covered

Confirmation is required before the first capital-moving leg of a full exit,
including an economic route switch, a move to 1x, a withdrawal that falls back
to full exit, or repayment of the whole debt from idle cash. A series of partial
repayments that belongs to a full-exit flow needs the same confirmation.

Debt-preserving partial withdrawals and risk reduction can continue under their
existing checks. NAV reports and cost-only forecasts remain automatic. Every
transaction retains its custody, budget, freshness and protocol checks.

The worker checks the approval at locked admission, before signing, and before
recording broadcast intent. Signed transactions retain the same approval
requirement across restarts.

## Confirm an ordinary exit

Use the existing privileged operator command `loyal-engine backyard
commit-unwind-intent`. It is a dry run unless `--execute` is supplied.
Executing now also requires `--confirmation-file <path>`.

The JSON file must contain exactly one object, use no unknown fields, and be
at most 4096 bytes. Its fields are:

| Field | Required value |
| --- | --- |
| `requestId` | A new lowercase SHA-256-format request ID; never reuse it for another exit. |
| `confirmedBy` | The operator recording the confirmation, nonempty and at most 256 characters. |
| `confirmationRecord` | The lowercase SHA-256 hash of the retained explicit confirmation record. |
| `acknowledgeUnavailableReborrow` | `true`, after acknowledging that reborrowing may be unavailable. |
| `expiresAt` | An RFC3339 timestamp in the future, at most 15 minutes from acceptance. |

The command's existing lane, purpose, observation, evidence, debt, collateral
and cost arguments define the approved exit. Review those bounds and the dry-run
output before executing. Use current observed evidence; do not guess amounts or
reuse an expired observation. Protect credentials through the existing systemd
credential mechanism.

This is a privileged-operator attestation. Host and database access authenticate
the privileged operator. The `confirmedBy` text and record hash are audit metadata. An automation agent must obtain the explicit confirmation before
creating or executing an ordinary approval; a deploy request is insufficient.

The receipt is retained in route state. An identical active request is
idempotent. A completed, changed or superseded request cannot authorize another
exit. Expiry or a larger required envelope needs a new confirmation. The ordinary
receipt history is capped at 128 entries and is never silently pruned.

## Emergency behavior

The worker creates emergency authority internally from fresh coherent account
observations. Utilization, an operator-supplied reason, withdrawal demand and a
saved `Fresh` flag do not establish an emergency.

Emergency authority is limited to the verified risk-reduction flow. After its
first leg reconciles, the remaining bounded exit may continue if LTV improves.
That authority cannot fund a new position, switch lanes or grant permission to
borrow again. Normal confirmations cannot disable a newly verified emergency.
Existing protocol emergency flags and genuine recovery safety stops still apply.

## Restart and rollout checks

A denied signed transaction retains its exact wire and reservation while it can
still land. Once finalized block height proves expiry, a subsequent signature
absence check can retire it automatically. This recovery does not wait for
operator confirmation. Already broadcast or ambiguous work keeps its existing
reconciliation path and is never blindly resent.

Emergency work must wait for a still-valid signed wire to reconcile or expire.
Database changes leave that wire valid on-chain. Do not
start a competing spend to avoid this wait.

Before rollout, review persisted unwind/leverage targets, approvals and signed
or submitted operations. Old unapproved signed work must not be grandfathered
into ordinary permission. Older worker binaries do not enforce this gate; a
rollback alone does not preserve its protection. Follow `deploy/hetzner/README.md`
for service operations. Keep host addresses and private infrastructure out of
this public repository.
