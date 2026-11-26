import type {
  BridgeDestination,
  BridgeDestinationToken,
  BridgeDestinationsResponse,
  BridgeOutQuoteResponse,
  BridgeRecipient,
  BridgeRecipientsResponse,
  BridgeTransfer,
  BridgeTransfersResponse,
} from "./generated/types.gen";

type Shape<T> = (body: unknown) => body is T;

const isRecord = (b: unknown): b is Record<string, unknown> =>
  typeof b === "object" && b !== null && !Array.isArray(b);

const isText = (v: unknown): v is string => typeof v === "string" && v !== "";

const isListOrNull = (v: unknown) => v === null || Array.isArray(v);

const TRANSFER_STATES = ["quoted", "submitted", "filled", "refunded", "failed"];

const TRANSFER_TEXT = [
  "requestId",
  "chain",
  "asset",
  "amountInRaw",
  "amountOutRaw",
  "quotedAt",
] as const;

const TRANSFER_TEXT_OR_NULL = [
  "originTx",
  "destinationTx",
  "refundTx",
  "confirmedAt",
  "submittedAt",
  "settledAt",
] as const;

export const isBridgeTransfer: Shape<BridgeTransfer> = (
  b
): b is BridgeTransfer =>
  isRecord(b) &&
  TRANSFER_TEXT.every((k) => isText(b[k])) &&
  TRANSFER_TEXT_OR_NULL.every((k) => b[k] === null || isText(b[k])) &&
  TRANSFER_STATES.includes(b.state as string) &&
  (b.direction === "in" || b.direction === "out") &&
  typeof b.inFlight === "boolean";

export const isBridgeOutQuote: Shape<BridgeOutQuoteResponse> = (
  b
): b is BridgeOutQuoteResponse =>
  isRecord(b) &&
  isText(b.requestId) &&
  Array.isArray(b.instructions) &&
  isListOrNull(b.lookupTables) &&
  isText(b.amountOutRaw) &&
  isText(b.amountOutMinRaw) &&
  isText(b.loyalFeeRaw) &&
  typeof b.loyalFeeBps === "number" &&
  typeof b.slippageBps === "number";

export const isBridgeTransfers: Shape<BridgeTransfersResponse> = (
  b
): b is BridgeTransfersResponse =>
  isRecord(b) &&
  isListOrNull(b.transfers) &&
  ((b.transfers as unknown[] | null) ?? []).every(isBridgeTransfer);

const isNonnegativeInteger = (v: unknown): v is number =>
  typeof v === "number" && Number.isSafeInteger(v) && v >= 0;

const isToken: Shape<BridgeDestinationToken> = (
  v
): v is BridgeDestinationToken =>
  isRecord(v) &&
  isText(v.asset) &&
  isText(v.symbol) &&
  isText(v.name) &&
  typeof v.icon === "string" &&
  isText(v.address) &&
  isNonnegativeInteger(v.minUsdc) &&
  isNonnegativeInteger(v.decimals) &&
  v.decimals <= 255;

const isDestination: Shape<BridgeDestination> = (v): v is BridgeDestination =>
  isRecord(v) &&
  isText(v.chain) &&
  isText(v.name) &&
  typeof v.icon === "string" &&
  typeof v.explorerTx === "string" &&
  (v.addressKind === "evm" || v.addressKind === "tron") &&
  isNonnegativeInteger(v.popularRank) &&
  (v.tokens === null || (Array.isArray(v.tokens) && v.tokens.every(isToken)));

const isRecipient: Shape<BridgeRecipient> = (v): v is BridgeRecipient =>
  isRecord(v) &&
  isText(v.address) &&
  isText(v.chain) &&
  isNonnegativeInteger(v.sends);

export const isBridgeDestinations: Shape<BridgeDestinationsResponse> = (
  b
): b is BridgeDestinationsResponse =>
  isRecord(b) &&
  (b.destinations === null ||
    (Array.isArray(b.destinations) && b.destinations.every(isDestination)));

export const isBridgeRecipients: Shape<BridgeRecipientsResponse> = (
  b
): b is BridgeRecipientsResponse =>
  isRecord(b) &&
  (b.recipients === null ||
    (Array.isArray(b.recipients) && b.recipients.every(isRecipient)));
