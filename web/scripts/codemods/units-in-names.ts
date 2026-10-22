import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { mkdirSync } from "node:fs";

type Op =
  | { kind: "replace"; from: string; to: string; all?: boolean }
  | { kind: "rename"; from: string; to: string }
  | { kind: "cut"; start: string; end: string }
  | { kind: "create"; content: string };

type FileOps = { file: string; ops: Op[] };

const ROOT = resolve(import.meta.dir, "../../..");

const replace = (from: string, to: string, all = false): Op => ({
  kind: "replace",
  from,
  to,
  all,
});
const rename = (from: string, to: string): Op => ({ kind: "rename", from, to });
const cut = (start: string, end: string): Op => ({ kind: "cut", start, end });
const create = (content: string): Op => ({ kind: "create", content });

const WEB = "web/apps/web";
const MOBILE = "web/apps/mobile";
const RELAY = "web/observability/telegram-relay/src";

const PLAN: FileOps[] = [
  {
    file: `${WEB}/src/components/chat-input.tsx`,
    ops: [
      replace(
        "const MAX_OFFSET = 37; // ~6px scaled up (6 * 980/160)\n",
        [
          "const DOG_VIEWBOX_WIDTH = 980;",
          "const DOG_VIEWBOX_HEIGHT = 784;",
          "const DOG_WIDTH_PX = 160;",
          "const PUPIL_MAX_OFFSET_PX = 6;",
          "const MAX_OFFSET = Math.round(",
          "  PUPIL_MAX_OFFSET_PX * (DOG_VIEWBOX_WIDTH / DOG_WIDTH_PX)",
          ");",
          "",
        ].join("\n")
      ),
      replace(
        "const scaleX = 980 / rect.width;\n      const scaleY = 784 / rect.height;",
        "const scaleX = DOG_VIEWBOX_WIDTH / rect.width;\n      const scaleY = DOG_VIEWBOX_HEIGHT / rect.height;"
      ),
      replace(
        '      width="160"\n      height="128"\n      viewBox="0 0 980 784"\n',
        '      width={DOG_WIDTH_PX}\n      height="128"\n      viewBox={`0 0 ${DOG_VIEWBOX_WIDTH} ${DOG_VIEWBOX_HEIGHT}`}\n'
      ),
      replace(
        "const MORPH_DURATION = 0.4; // seconds",
        "const MORPH_DURATION_SECONDS = 0.4;"
      ),
      rename("MORPH_DURATION", "MORPH_DURATION_SECONDS"),
    ],
  },
  {
    file: `${WEB}/src/components/wallet-sidebar/send-content.tsx`,
    ops: [
      replace(
        'const red = "#F9363C";\n',
        'const red = "#F9363C";\nconst VAULT_RENT_EXEMPT_CUSHION_SOL = 0.001;\nconst WALLET_FEE_RESERVE_SOL = 0.00005;\n'
      ),
      replace(
        [
          "        // Vault PDAs must keep a rent-exempt minimum (~0.00089 SOL for a",
          "        // bare system account); leave a slightly larger cushion so the",
          '        // multisig transfer doesn\'t fail with "insufficient funds for rent".',
          "        // For User-wallet sends only the per-tx fee needs to stay behind.",
          "        const reserve = vaultContext ? 0.001 : 0.00005;",
        ].join("\n"),
        [
          "        const reserve = vaultContext",
          "          ? VAULT_RENT_EXEMPT_CUSHION_SOL",
          "          : WALLET_FEE_RESERVE_SOL;",
        ].join("\n")
      ),
    ],
  },
  {
    file: `${WEB}/src/components/wallet-workspace/facelift/use-earn-actions.ts`,
    ops: [
      replace(
        "export function useEarnActions(deps: {",
        "const PENDING_SIGNATURE_CAP_MS = 20_000;\n\nexport function useEarnActions(deps: {"
      ),
      replace(
        "    // ponytail: 20s cap — the indexer lag is ~5s, and a mismatch must never\n    // pin a skeleton row for long.\n",
        ""
      ),
      replace(
        "        current.filter((existing) => existing !== signature)\n      );\n    }, 20_000);",
        "        current.filter((existing) => existing !== signature)\n      );\n    }, PENDING_SIGNATURE_CAP_MS);"
      ),
    ],
  },
  {
    file: `${WEB}/src/hooks/use-swap.ts`,
    ops: [
      replace(
        "const SLIPPAGE_BPS = 50;\n",
        "const SLIPPAGE_BPS = 50;\nconst PRIORITY_FEE_MAX_LAMPORTS = 50_000_000;\n"
      ),
      replace(
        "maxLamports: 50_000_000, // 0.05 SOL max for priority",
        "maxLamports: PRIORITY_FEE_MAX_LAMPORTS,"
      ),
    ],
  },
  {
    file: `${WEB}/scripts/verify-earn-withdraw-tx-size.ts`,
    ops: [
      replace(
        "const PACKET_DATA_SIZE = 1232;\n",
        [
          "const PACKET_DATA_SIZE = 1232;",
          "const MESSAGE_VERSION_PREFIX_BYTES = 1;",
          "const MESSAGE_HEADER_BYTES = 3;",
          "const PUBKEY_BYTES = 32;",
          "const BLOCKHASH_BYTES = 32;",
          "const PROGRAM_ID_INDEX_BYTES = 1;",
          "",
        ].join("\n")
      ),
      replace(
        "let length = 1 + 3; // version prefix + header",
        "let length = MESSAGE_VERSION_PREFIX_BYTES + MESSAGE_HEADER_BYTES;"
      ),
      replace(
        "length += 32 * message.staticAccountKeys.length;",
        "length += PUBKEY_BYTES * message.staticAccountKeys.length;"
      ),
      replace("length += 32; // recentBlockhash", "length += BLOCKHASH_BYTES;"),
      replace(
        "length += 1; // programIdIndex",
        "length += PROGRAM_ID_INDEX_BYTES;"
      ),
      replace("length += 32; // table account key", "length += PUBKEY_BYTES;"),
    ],
  },
  {
    file: `${WEB}/src/features/earn-max/voltr/apy.server.ts`,
    ops: [
      replace(
        'import { VOLTR_VAULT } from "@loyal-labs/smart-account-vaults";',
        'import {\n  VOLTR_ASSET_DECIMALS,\n  VOLTR_VAULT,\n} from "@loyal-labs/smart-account-vaults";'
      ),
      replace(
        "const DAY_MS = 86_400_000;\n",
        "const DAY_MS = 86_400_000;\nconst VOLTR_LP_DECIMALS = 9;\nconst SHARE_PRICE_SCALE = 10 ** (VOLTR_LP_DECIMALS - VOLTR_ASSET_DECIMALS);\n"
      ),
      replace(
        "    // sharePrice is per 1 LP unit scaled by 1e9/1e6 decimals; derive live LP\n    // from it so both points use the same TVL/LP definition.\n",
        ""
      ),
      replace(
        "live.totalValue / (live.sharePrice / 1000)",
        "live.totalValue / (live.sharePrice / SHARE_PRICE_SCALE)"
      ),
    ],
  },
  {
    file: `${WEB}/src/features/funding/server/evm-deposit.ts`,
    ops: [
      replace(
        "  /** Unix ms; must be identical when signing and when sending. */\n  requestExpiry: number\n) {",
        "  requestExpiryUnixMs: number\n) {"
      ),
      replace(
        '"privy-request-expiry": String(requestExpiry),',
        '"privy-request-expiry": String(requestExpiryUnixMs),'
      ),
    ],
  },
  {
    file: `${WEB}/src/app/api/cron/solana-week-quest-completions/route.ts`,
    ops: [
      replace(
        "const DEFAULT_SWEEP_LOOKBACK_HOURS = 168; // 7 days — covers a week-long event.",
        "const SOLANA_WEEK_EVENT_DAYS = 7;\nconst DEFAULT_SWEEP_LOOKBACK_HOURS = SOLANA_WEEK_EVENT_DAYS * 24;"
      ),
    ],
  },
  {
    file: `${WEB}/src/lib/kamino/earn-forecast.server.ts`,
    ops: [
      replace(
        "// Extra lookback so the point just before the first series hour's window\n// start is not dropped, which would null the first realized-series sample(s).\n",
        ""
      ),
      rename("HISTORY_LOOKBACK_SLACK_MS", "PRE_WINDOW_POINT_LOOKBACK_MS"),
    ],
  },
  {
    file: `${WEB}/src/lib/kamino/earn-reserve-share-price-repository.server.ts`,
    ops: [
      replace(
        "// A sample stays in force until the next one, for at most six hours, so the\n// allocation at the start of the window can come from just before it.\n",
        ""
      ),
      rename("ALLOCATION_SEED_LOOKBACK_MS", "ALLOCATION_SAMPLE_MAX_HOLD_MS"),
    ],
  },
  {
    file: `${MOBILE}/src/lib/wallet/mwa-signer.ts`,
    ops: [
      replace(
        'import type { Web3MobileWallet } from "@solana-mobile/mobile-wallet-adapter-protocol-web3js";\n',
        'import type { SolanaMobileWalletAdapterProtocolErrorCode } from "@solana-mobile/mobile-wallet-adapter-protocol";\nimport type { Web3MobileWallet } from "@solana-mobile/mobile-wallet-adapter-protocol-web3js";\n'
      ),
      replace(
        "function isUserCancellation(error: unknown): boolean {",
        [
          "const MWA_ERROR = {",
          "  ERROR_AUTHORIZATION_FAILED: -1,",
          "  ERROR_NOT_SIGNED: -3,",
          "} as const satisfies Pick<",
          "  typeof SolanaMobileWalletAdapterProtocolErrorCode,",
          '  "ERROR_AUTHORIZATION_FAILED" | "ERROR_NOT_SIGNED"',
          ">;",
          "",
          "function isUserCancellation(error: unknown): boolean {",
        ].join("\n")
      ),
      replace(
        "hasErrorCode(error, -1) // ERROR_AUTHORIZATION_FAILED",
        "hasErrorCode(error, MWA_ERROR.ERROR_AUTHORIZATION_FAILED)"
      ),
      replace(
        "if (hasErrorCode(error, -3)) {\n        // ERROR_NOT_SIGNED: the user tapped decline in the wallet app.\n",
        "if (hasErrorCode(error, MWA_ERROR.ERROR_NOT_SIGNED)) {\n"
      ),
      replace(
        "hasErrorCode(error, -1)",
        "hasErrorCode(error, MWA_ERROR.ERROR_AUTHORIZATION_FAILED)",
        true
      ),
    ],
  },
  {
    file: `${MOBILE}/src/lib/wallet/vault-account-storage.ts`,
    ops: [
      replace(
        [
          "/**",
          " * Persistent record of a Seed Vault authorization. The seed itself stays",
          " * inside the vault; we only need to remember the auth token, which",
          " * derivation path we authorized, and the resolved base58 Solana address",
          " * so the wallet can display it before any signing operation runs.",
          " *",
          " * `authToken`",
        ].join("\n"),
        "/**\n * `authToken`"
      ),
    ],
  },
  {
    file: `${MOBILE}/src/components/earn/EarnChart.tsx`,
    ops: [
      replace(
        [
          "const SMALLEST_PLACE = 0.00001; // the 5th decimal",
          "const WHOLE_FACTOR = 100_000; // whole dollars",
          "const DECIMAL_FACTORS = [10_000, 1000, 100, 10, 1]; // .1 → .00001",
        ].join("\n"),
        [
          "const DISPLAY_DECIMALS = 5;",
          "const WHOLE_FACTOR = 10 ** DISPLAY_DECIMALS;",
          "const SMALLEST_PLACE = 1 / WHOLE_FACTOR;",
          "const DECIMAL_FACTORS = Array.from(",
          "  { length: DISPLAY_DECIMALS },",
          "  (_, place) => WHOLE_FACTOR / 10 ** (place + 1),",
          ");",
        ].join("\n")
      ),
    ],
  },
  {
    file: `${MOBILE}/src/components/quests/QuestCompleteNotification.tsx`,
    ops: [
      replace(
        [
          "// White panel anatomy (drives both the panel min-height and where the dog",
          "// snout meets it). pt 72 + title 80 + gap 16 + subtitle 48 + gap 48 + button 50.",
          "const BUTTON_HEIGHT = 50;",
          "const PANEL_PT = 72;",
          "const PANEL_CONTENT = PANEL_PT + 80 + 16 + 48 + 48 + BUTTON_HEIGHT;",
        ].join("\n"),
        [
          "const TITLE_HEIGHT = 80;",
          "const TITLE_GAP = 16;",
          "const SUBTITLE_HEIGHT = 48;",
          "const BUTTON_GAP = 48;",
          "const BUTTON_HEIGHT = 50;",
          "const PANEL_PT = 72;",
          "const PANEL_CONTENT =",
          "  PANEL_PT +",
          "  TITLE_HEIGHT +",
          "  TITLE_GAP +",
          "  SUBTITLE_HEIGHT +",
          "  BUTTON_GAP +",
          "  BUTTON_HEIGHT;",
        ].join("\n")
      ),
      replace(
        '                marginTop: 16,\n                fontFamily: "Geist_400Regular",',
        '                marginTop: TITLE_GAP,\n                fontFamily: "Geist_400Regular",'
      ),
      replace(
        "                marginTop: 48,\n                height: BUTTON_HEIGHT,",
        "                marginTop: BUTTON_GAP,\n                height: BUTTON_HEIGHT,"
      ),
    ],
  },
  {
    file: `${MOBILE}/src/components/wallet/SendSheet.tsx`,
    ops: [
      replace(
        'const INVALID_RECIPIENT_HINT = "Invalid Solana address";\n',
        'const INVALID_RECIPIENT_HINT = "Invalid Solana address";\nconst AMOUNT_ROW_PADDING_X = 16;\nconst SWAP_BUTTON_WIDTH = 52;\n'
      ),
      replace(
        "SCREEN_WIDTH - 32 - 52, // px-16*2 + swap button",
        "SCREEN_WIDTH - 2 * AMOUNT_ROW_PADDING_X - SWAP_BUTTON_WIDTH,"
      ),
      replace(
        "      <View style={{ paddingTop: 36, paddingHorizontal: 16 }}>\n        <Pressable style={amountFieldStyles.row} onPress={openKeyboard}>",
        "      <View style={{ paddingTop: 36, paddingHorizontal: AMOUNT_ROW_PADDING_X }}>\n        <Pressable style={amountFieldStyles.row} onPress={openKeyboard}>"
      ),
      replace(
        '{ paddingRight: 52, pointerEvents: "none" },',
        '{ paddingRight: SWAP_BUTTON_WIDTH, pointerEvents: "none" },'
      ),
    ],
  },
  {
    file: `${MOBILE}/src/features/activity/model/ActivityProvider.tsx`,
    ops: [
      replace(
        [
          "// Cadence + bound for polling a due scheduled sweep to resolution. Short ticks so",
          "// the row clears promptly after an execute — the server-side reconcile drops it on",
          "// the next read once the worker has reverted/failed the slot. Capped (~40s) so a",
          "// backend-stuck slot can't poll forever (a manual pull works after that). The",
          "// floor on perceived latency is the worker's async revert, not this interval.",
          "const DUE_SWEEP_POLL_MS = 2000;",
          "const MAX_DUE_SWEEP_POLLS = 20;",
          "",
          '// After "Execute now", the worker\'s resulting `balance_sweep` tx can lag behind',
          "// the slot clearing. Poll the Earn feed for it on a short cadence so the morph",
          "// can swap to it promptly, bounded (~60s) so a stuck/no-op execution can't poll",
          "// forever — the executing row just lingers until a manual pull / blur.",
          "const SWEEP_RESULT_POLL_MS = 2500;",
          "const MAX_SWEEP_RESULT_POLLS = 24;",
          "// Hard cap on how long a morph stays pinned (incl. after it resolves) so it",
          "// can't hold a row out of its normal date group indefinitely if the user never",
          "// leaves the screen.",
          "const SWEEP_MORPH_MAX_MS = 120_000;",
        ].join("\n"),
        [
          "const DUE_SWEEP_POLL_MS = 2000;",
          "const DUE_SWEEP_POLL_BUDGET_MS = 40_000;",
          "const MAX_DUE_SWEEP_POLLS = DUE_SWEEP_POLL_BUDGET_MS / DUE_SWEEP_POLL_MS;",
          "",
          "const SWEEP_RESULT_POLL_MS = 2500;",
          "const SWEEP_RESULT_POLL_BUDGET_MS = 60_000;",
          "const MAX_SWEEP_RESULT_POLLS =",
          "  SWEEP_RESULT_POLL_BUDGET_MS / SWEEP_RESULT_POLL_MS;",
          "const SWEEP_MORPH_MAX_PINNED_MS = 120_000;",
        ].join("\n")
      ),
      rename("SWEEP_MORPH_MAX_MS", "SWEEP_MORPH_MAX_PINNED_MS"),
    ],
  },
  {
    file: "web/apps/admin/src/app/(admin)/vault-integrations/earn-max/page.tsx",
    ops: [
      replace(
        "// Routine NAV reports are hourly; allow 15 minutes for reporting/reconciliation.\nconst STALE_REPORT_S = 75 * 60;",
        "const HOURLY_REPORT_S = 60 * 60;\nconst REPORT_GRACE_S = 15 * 60;\nconst STALE_REPORT_S = HOURLY_REPORT_S + REPORT_GRACE_S;"
      ),
    ],
  },
  {
    file: "web/apps/extension/src/lib/brands.ts",
    ops: [
      create(
        [
          'export type Base64 = string & { readonly __brand: "Base64" };',
          "",
          "export function uint8ArrayToBase64(bytes: Uint8Array): Base64 {",
          '  let binary = "";',
          "  for (const byte of bytes) binary += String.fromCharCode(byte);",
          "  return btoa(binary) as Base64;",
          "}",
          "",
        ].join("\n")
      ),
    ],
  },
  {
    file: "web/apps/extension/entrypoints/background.ts",
    ops: [
      replace(
        '} from "~/src/lib/dapp-messages";\n',
        '} from "~/src/lib/dapp-messages";\nimport { type Base64, uint8ArrayToBase64 } from "~/src/lib/brands";\n'
      ),
      replace(
        "  transaction?: string; // base64\n  message?: string; // base64\n",
        "  transaction?: Base64;\n  message?: Base64;\n"
      ),
      replace(
        [
          "function uint8ArrayToBase64(bytes: Uint8Array): string {",
          '  let binary = "";',
          "  for (const byte of bytes) binary += String.fromCharCode(byte);",
          "  return btoa(binary);",
          "}",
          "",
          "",
        ].join("\n"),
        ""
      ),
    ],
  },
  {
    file: "web/apps/extension/src/lib/dapp-messages.ts",
    ops: [
      replace(
        "export interface DappConnectRequest {",
        'import type { Base64 } from "./brands";\n\nexport interface DappConnectRequest {'
      ),
      replace(
        "  // base64-encoded serialized transaction\n  transaction: string;\n",
        "  transaction: Base64;\n"
      ),
      replace(
        "  // base64-encoded signed transaction\n  signedTransaction?: string;\n",
        "  signedTransaction?: Base64;\n"
      ),
      replace(
        "  // base64-encoded message bytes\n  message: string;\n",
        "  message: Base64;\n"
      ),
      replace(
        "  // base64-encoded signature\n  signature?: string;\n",
        "  signature?: Base64;\n"
      ),
    ],
  },
  {
    file: "web/apps/telegram/src/app/page.tsx",
    ops: [
      replace(
        "const SPLASH_DURATION = 2400; // ms - time before redirect",
        "const SPLASH_DURATION_MS = 2400;"
      ),
      rename("SPLASH_DURATION", "SPLASH_DURATION_MS"),
    ],
  },
  {
    file: "go/workers/internal/chain/client.go",
    ops: [
      replace(
        "// PriorityFees are the per-compute-unit prices, in micro-lamports, that\n// recent landed transactions writing any of writable paid, one per slot.\nfunc (c *Client) PriorityFees(ctx context.Context, writable []solana.PublicKey) ([]uint64, error) {",
        "type MicroLamports uint64\n\n// PriorityFees are the per-compute-unit prices that recent landed\n// transactions writing any of writable paid, one per slot.\nfunc (c *Client) PriorityFees(ctx context.Context, writable []solana.PublicKey) ([]MicroLamports, error) {"
      ),
      replace(
        "\tfees := make([]uint64, len(out))\n\tfor i, row := range out {\n\t\tfees[i] = row.PrioritizationFee\n\t}",
        "\tfees := make([]MicroLamports, len(out))\n\tfor i, row := range out {\n\t\tfees[i] = MicroLamports(row.PrioritizationFee)\n\t}"
      ),
    ],
  },
  {
    file: "go/workers/internal/fleet/chain.go",
    ops: [
      replace(
        "\treturn fees[(len(fees)*75+99)/100-1], nil",
        "\treturn uint64(fees[(len(fees)*75+99)/100-1]), nil"
      ),
    ],
  },
  {
    file: "go/workers/internal/programs/meteora/meteora.go",
    ops: [
      replace(
        "// PriceOfBin is the price of one lamport of token X in lamports of token Y.\nfunc PriceOfBin(binID int32, binStep uint16) float64 {\n\treturn math.Pow(1+float64(binStep)/basisPointMax, float64(binID))",
        "type RawPriceYPerX float64\n\nfunc PriceOfBin(binID int32, binStep uint16) RawPriceYPerX {\n\treturn RawPriceYPerX(math.Pow(1+float64(binStep)/basisPointMax, float64(binID)))"
      ),
    ],
  },
  {
    file: "go/workers/internal/admin/autonomous_vault.go",
    ops: [
      replace(
        "price := meteora.PriceOfBin(pool.ActiveID, pool.BinStep) * math.Pow10(",
        "price := float64(meteora.PriceOfBin(pool.ActiveID, pool.BinStep)) * math.Pow10("
      ),
    ],
  },
  {
    file: "web/packages/loyal-actions/src/internal/squads.ts",
    ops: [
      replace(
        'const SQUADS_SEED_POLICY = new TextEncoder().encode("policy");\n',
        'const SQUADS_SEED_POLICY = new TextEncoder().encode("policy");\nconst SQUADS_PERIOD_V2_DAILY = 1;\n'
      ),
      replace("    // Squads PeriodV2::Daily.\n", ""),
      replace(
        "encoder.pushU8(1);\n    encoder.pushU64(spendingLimit.maxPerPeriod);",
        "encoder.pushU8(SQUADS_PERIOD_V2_DAILY);\n    encoder.pushU64(spendingLimit.maxPerPeriod);",
        true
      ),
    ],
  },
  {
    file: "web/packages/loyal-smart-accounts-core/src/utils.ts",
    ops: [
      replace(
        "const STRING_LEN_SIZE = 4;\n",
        "const STRING_LEN_SIZE = 4;\nconst COMPACT_U16_LENGTH_HEADROOM_BYTES = 1;\n"
      ),
      replace(
        "    STRING_LEN_SIZE -\n    // Sometimes long memo can trigger switching from 1 to 2 bytes length encoding in Compact-u16,\n    // so we reserve 1 extra byte to make sure.\n    1\n",
        "    STRING_LEN_SIZE -\n    COMPACT_U16_LENGTH_HEADROOM_BYTES\n"
      ),
    ],
  },
  {
    file: "web/packages/smart-account-vaults/src/voltr/plans.ts",
    ops: [
      replace(
        "const RECEIPT_SPACE = 112;\n",
        "const RECEIPT_SPACE = 112;\nconst CLAIM_ROUNDING_HEADROOM_RAW = BigInt(1);\n"
      ),
      replace(
        "  // One raw unit of headroom absorbs program-side fixed-point rounding; any\n  // dust stays in the vault account and rides the next sweep.\n",
        ""
      ),
      replace(
        "pending.payoutRaw > BigInt(0) ? pending.payoutRaw - BigInt(1) : BigInt(0);",
        "pending.payoutRaw > BigInt(0)\n      ? pending.payoutRaw - CLAIM_ROUNDING_HEADROOM_RAW\n      : BigInt(0);"
      ),
    ],
  },
  {
    file: "web/packages/smart-account-vaults/src/voltr/program.ts",
    ops: [
      replace(
        "export const EARN_MAX_VOLTR_ACCOUNT_INDEX = 2;\n",
        [
          "export const EARN_MAX_VOLTR_ACCOUNT_INDEX = 2;",
          "const IS_AMOUNT_IN_LP = 1;",
          "const VAULT_TOTAL_VALUE_OFFSET = 168;",
          "const VAULT_LOCKED_PROFIT_DURATION_OFFSET = 448;",
          "const VAULT_LOCKED_PROFIT_OFFSET = 672;",
          "const VAULT_LAST_REPORT_OFFSET = 680;",
          "",
        ].join("\n")
      ),
      replace(
        "      // isAmountInLp = true, the only mode our callers and Voltr tooling use.\n      Buffer.from([1, args.withdrawAll ? 1 : 0]),",
        "      Buffer.from([IS_AMOUNT_IN_LP, args.withdrawAll ? 1 : 0]),"
      ),
      replace(
        "  BigInt(data.readUInt16LE(offset));\n",
        "  BigInt(data.readUInt16LE(offset));\nconst readU80F48 = (data: Buffer, offset: number) =>\n  (readU64(data, offset) | (readU64(data, offset + 8) << BigInt(64))) >>\n  BigInt(48);\n"
      ),
      replace(
        [
          "  // Locked profit decays linearly from lastReport over the degradation window.",
          "  const lockedDuration = readU64(v, 448);",
          "  const lockedAmount = readU64(v, 672);",
          "  const sinceReport = nowSec - readU64(v, 680);",
        ].join("\n"),
        [
          "  const lockedDuration = readU64(v, VAULT_LOCKED_PROFIT_DURATION_OFFSET);",
          "  const lockedAmount = readU64(v, VAULT_LOCKED_PROFIT_OFFSET);",
          "  const sinceReport = nowSec - readU64(v, VAULT_LAST_REPORT_OFFSET);",
        ].join("\n")
      ),
      replace(
        "const unlockedValue = readU64(v, 168) - lockedProfit;",
        "const unlockedValue = readU64(v, VAULT_TOTAL_VALUE_OFFSET) - lockedProfit;"
      ),
      replace(
        "    // amountAssetToWithdrawDecimalBits is U80F48 in raw asset units.\n    const atRequest =\n      (readU64(r, 80) | (readU64(r, 88) << BigInt(64))) >> BigInt(48);",
        "    const atRequest = readU80F48(r, 80);"
      ),
    ],
  },
  {
    file: "web/packages/smart-account-vaults/src/wallet.ts",
    ops: [
      replace(
        "async function getSimulationDiagnosticError(args: {",
        [
          "const WALLET_USER_REJECTED_CODE = 4001;",
          "",
          "function isUserRejection(error: unknown): boolean {",
          "  return (",
          '    typeof error === "object" &&',
          "    error !== null &&",
          '    "code" in error &&',
          "    error.code === WALLET_USER_REJECTED_CODE",
          "  );",
          "}",
          "",
          "async function getSimulationDiagnosticError(args: {",
        ].join("\n")
      ),
      replace(
        [
          "  // An explicit user denial must not become a failure of a dependent transaction.",
          "  if (",
          "    args.error &&",
          '    typeof args.error === "object" &&',
          '    "code" in args.error &&',
          "    args.error.code === 4001",
          "  ) {",
        ].join("\n"),
        "  if (isUserRejection(args.error)) {"
      ),
    ],
  },
  {
    file: "web/observability/scripts/smoke-live.sh",
    ops: [
      replace(
        [
          "# otel_logs has a 30-day TTL. A marker older than that is deleted by design, so",
          "# looking it up would fail every restart. Renew it well before the TTL.",
          'if [ -s "$marker_file" ] && [ -z "$(find "$marker_file" -mtime +25)" ]; then',
        ].join("\n"),
        [
          "otel_logs_ttl_days=30",
          "marker_renew_after_days=$((otel_logs_ttl_days - 5))",
          'if [ -s "$marker_file" ] && [ -z "$(find "$marker_file" -mtime +"$marker_renew_after_days")" ]; then',
        ].join("\n")
      ),
    ],
  },
  {
    file: `${RELAY}/app.ts`,
    ops: [
      replace(
        "  /** Minutes past UTC midnight at which the daily recap is posted. */\n",
        ""
      ),
      rename("dailyRecapAtMinutes", "dailyRecapAtUtcMinutes"),
      replace('/** "HH:MM" in UTC, as minutes past midnight. */\n', ""),
      rename("clockMinutes", "utcClockMinutes"),
      cut(
        "/**\n * Logs the message text before it is handed to the destination",
        "function createOutgoingMessageLogger("
      ),
    ],
  },
  {
    file: `${RELAY}/index.ts`,
    ops: [
      replace(
        "dailyRecapAtMinutes: config.dailyRecapAtMinutes,",
        "dailyRecapAtMinutes: config.dailyRecapAtUtcMinutes,"
      ),
    ],
  },
  {
    file: `${RELAY}/e2e.test.ts`,
    ops: [
      replace(
        "config.dailyRecapAtMinutes",
        "config.dailyRecapAtUtcMinutes",
        true
      ),
    ],
  },
  {
    file: `${RELAY}/format.ts`,
    ops: [
      replace(
        '/** Headroom for the "and N more" footer while rows are being fitted. */\n',
        ""
      ),
      rename("FOOTER_RESERVE", "FOOTER_RESERVE_CHARS"),
    ],
  },
  {
    file: "web/packages/shared/src/dust-filter.ts",
    ops: [
      replace(
        "  /** Absolute raw token amount (post-decimal). Must be non-negative. */\n  rawAmount: bigint;",
        "  absRaw: bigint;"
      ),
      replace("input.rawAmount", "input.absRaw", true),
    ],
  },
  {
    file: `${MOBILE}/src/lib/solana/rpc/get-account-txn-history.ts`,
    ops: [
      replace("rawAmount: tokenChange.absRaw,", "absRaw: tokenChange.absRaw,"),
    ],
  },
  {
    file: "web/packages/solana-wallet/src/parsers/activity-parser.ts",
    ops: [
      replace("rawAmount: tokenChange.absRaw,", "absRaw: tokenChange.absRaw,"),
    ],
  },
  {
    file: "web/packages/shared/src/earn-max-history.ts",
    ops: [
      replace(
        "const EARNED_WINDOW_DAYS = 30;\n",
        "const EARNED_WINDOW_DAYS = 30;\nconst ROUNDING_DUST_USD = 0.01;\n"
      ),
      replace(
        "// UTC days: Voltr's daily share prices close at 00:00 UTC, so a local-day\n// split would show each day's yield on the next local day east of UTC.\n",
        ""
      ),
      rename("dayKey", "utcDayKey"),
      replace(
        "    // Deposit rounding dust (< 1 cent) is not a loss; real losses still chart.\n    const earned = raw < 0 && raw > -0.01 ? 0 : raw;",
        "    const earned = raw < 0 && raw > -ROUNDING_DUST_USD ? 0 : raw;"
      ),
    ],
  },
  {
    file: "web/packages/solana-wallet/src/providers/default-activity-provider.ts",
    ops: [
      replace(
        "export function createRpcActivityProvider(args: {",
        "const SIGNATURE_OVERFETCH_FACTOR = 2;\nconst MIN_SIGNATURE_BATCH_SIZE = 30;\nconst MAX_SIGNATURE_PAGES = 3;\n\nexport function createRpcActivityProvider(args: {"
      ),
      replace(
        [
          "    // Fetch more signatures per RPC call to account for filtered-out noise.",
          "    // Keep batch size moderate to avoid 429 rate limits from the RPC.",
          "    const batchSize = Math.max(desiredCount * 2, 30);",
          "    const maxPages = 3;",
          "",
        ].join("\n"),
        [
          "    const batchSize = Math.max(",
          "      desiredCount * SIGNATURE_OVERFETCH_FACTOR,",
          "      MIN_SIGNATURE_BATCH_SIZE",
          "    );",
          "",
        ].join("\n")
      ),
      rename("maxPages", "MAX_SIGNATURE_PAGES"),
    ],
  },
  {
    file: "web/packages/wallet-core/src/hooks/use-wallet-data.ts",
    ops: [
      replace(
        'const LOYL_MINT = "LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta";\n',
        'const LOYL_MINT = "LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta";\nconst LOYL_PINNED_INDEX = 2;\n'
      ),
      replace(
        [
          "    // Only LOYAL is pinned regardless of balance (3rd position / index 2);",
          "    // every other token appears solely when it has a non-zero balance, so real",
          "    // holdings fill the top instead of zero SOL/USDC placeholders.",
          "",
        ].join("\n"),
        ""
      ),
      replace(
        "Math.min(2, rows.length)",
        "Math.min(LOYL_PINNED_INDEX, rows.length)",
        true
      ),
      rename("rawTimestamp", "rawTimestampMs"),
    ],
  },
  {
    file: "web/packages/wallet-core/src/types/wallet.ts",
    ops: [
      replace(
        "  /** Epoch milliseconds for sort order — absent in legacy localStorage rows */\n",
        ""
      ),
      rename("rawTimestamp", "rawTimestampMs"),
    ],
  },
  ...[
    `${WEB}/src/components/wallet-workspace/facelift/sidebar.tsx`,
    `${WEB}/src/hooks/use-wallet-desktop-data.ts`,
    `${WEB}/src/hooks/use-smart-account-sidebar-data.ts`,
  ].map((file) => ({ file, ops: [rename("rawTimestamp", "rawTimestampMs")] })),
];

function countOf(text: string, needle: string): number {
  return text.split(needle).length - 1;
}

const problems: string[] = [];
let applied = 0;
let skipped = 0;

for (const { file, ops } of PLAN) {
  const path = resolve(ROOT, file);
  if (ops.length === 1 && ops[0].kind === "create") {
    const want = ops[0].content;
    if (existsSync(path) && readFileSync(path, "utf8") === want) {
      skipped += 1;
    } else if (existsSync(path)) {
      problems.push(`${file}: exists with different content`);
    } else {
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, want);
      applied += 1;
    }
    continue;
  }
  let text = readFileSync(path, "utf8");
  const before = text;
  for (const op of ops) {
    if (op.kind === "replace") {
      if (!op.all && op.to !== "" && text.includes(op.to)) {
        skipped += 1;
        continue;
      }
      const n = countOf(text, op.from);
      if (n === 0) {
        if (op.to === "" || text.includes(op.to)) skipped += 1;
        else problems.push(`${file}: not found: ${op.from.slice(0, 70)}`);
        continue;
      }
      if (n > 1 && !op.all) {
        problems.push(`${file}: ${n} matches: ${op.from.slice(0, 70)}`);
        continue;
      }
      text = op.all
        ? text.split(op.from).join(op.to)
        : text.replace(op.from, () => op.to);
      applied += 1;
    } else if (op.kind === "rename") {
      const pattern = new RegExp(`\\b${op.from}\\b`, "g");
      if (pattern.test(text)) {
        text = text.replace(pattern, op.to);
        applied += 1;
      } else if (text.includes(op.to)) {
        skipped += 1;
      } else {
        problems.push(`${file}: identifier not found: ${op.from}`);
      }
    } else if (op.kind === "cut") {
      const start = text.indexOf(op.start);
      if (start < 0) {
        skipped += 1;
        continue;
      }
      const end = text.indexOf(op.end, start);
      if (end < 0) {
        problems.push(`${file}: cut end not found: ${op.end}`);
        continue;
      }
      text = text.slice(0, start) + text.slice(end);
      applied += 1;
    }
  }
  if (text !== before) writeFileSync(path, text);
}

console.log(`applied ${applied}, already done ${skipped}`);
if (problems.length > 0) {
  console.error(problems.join("\n"));
  process.exit(1);
}
