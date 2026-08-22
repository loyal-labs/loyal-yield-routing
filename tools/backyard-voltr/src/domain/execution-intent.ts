import { createHash } from "node:crypto";

import type { Address } from "@solana/kit";

import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_ROUTE, routeSpecSha256, type PartnerRouteSpec } from "./route-spec.js";

export type SetupOperation =
  | "initialize-vault"
  | "initialize-vault-and-adaptor"
  | "add-adaptor"
  | "initialize-strategy"
  | "initialize-strategy-asset-ata"
  | "install-deposit-policy"
  | "install-withdraw-policy";

export type RuntimeOperation =
  | "user-deposit"
  | "instant-withdraw"
  | "manager-deposit"
  | "manager-withdraw"
  | "withdraw-request"
  | "withdraw-claim";

type IntentBase = Readonly<{
  schemaVersion: 1;
  routeId: PartnerRouteSpec["id"] | typeof PARTNER_FOUR_MARKET_ROUTE.id;
  routeSpecSha256: string;
  nonce: string;
  prestateSlot: bigint;
  expiresAtUnix: bigint;
  canonicalMessageSha256: string;
  /** Bound to one coherent lifecycle when the command is used for evidence. */
  lifecycleId?: string;
  /** Hash of the exact protected prestate envelope used to authorize a send. */
  protectedPrestateSha256?: string;
}>;

export type SetupIntent = IntentBase & Readonly<{
  kind: "setup";
  operation: SetupOperation;
  signer: Address;
}>;

export type UserRuntimeIntent = IntentBase & Readonly<{
  kind: "runtime";
  operation: "user-deposit" | "instant-withdraw" | "withdraw-request" | "withdraw-claim";
  signerRole: "user";
  user: Address;
  amountRaw: bigint;
  /** Explicit lifecycle nonce supplied by the operator for every user leg. */
  lifecycleId: string;
  /** Exact hash of the protected route-owned prestate read before signing. */
  protectedPrestateSha256: string;
}>;

export type ManagerRuntimeIntent = IntentBase & Readonly<{
  kind: "runtime";
  operation: "manager-deposit" | "manager-withdraw";
  signerRole: "guardian";
  guardian: Address;
  policy: Address;
  amountRaw: bigint;
  lifecycleId: string;
  protectedPrestateSha256: string;
  routeAuthorizationSha256: string;
}>;

export type ExecutionIntent = SetupIntent | UserRuntimeIntent | ManagerRuntimeIntent;

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function intentSha256(intent: ExecutionIntent): string {
  return createHash("sha256").update(canonicalJson(intent)).digest("hex");
}

export function assertIntentForRoute(
  intent: ExecutionIntent,
  route: PartnerRouteSpec = PARTNER_ROUTE,
): void {
  assertIntentForRouteBinding(intent, {
    routeId: route.id,
    routeSpecSha256: routeSpecSha256(route),
    maxManagerOperationRaw: route.asset.maxManagerOperationRaw,
  });
}

export function assertIntentForRouteBinding(
  intent: ExecutionIntent,
  binding: Readonly<{
    routeId: string;
    routeSpecSha256: string;
    maxManagerOperationRaw: bigint;
    routeAuthorizationSha256?: string;
  }>,
): void {
  if (intent.routeId !== binding.routeId || intent.routeSpecSha256 !== binding.routeSpecSha256) {
    throw new Error("execution intent is not bound to the exact route specification");
  }
  if (!/^[0-9a-f]{64}$/.test(intent.canonicalMessageSha256)) {
    throw new Error("execution intent canonical message hash is malformed");
  }
  if (intent.expiresAtUnix <= 0n || intent.prestateSlot <= 0n || intent.nonce.length < 16) {
    throw new Error("execution intent freshness fields are invalid");
  }
  if (intent.kind === "runtime" && intent.amountRaw <= 0n) {
    throw new Error("runtime amount must be positive");
  }
  if (intent.kind === "runtime" && intent.signerRole === "user") {
    if (!/^[0-9a-f]{64}$/.test(intent.lifecycleId)) {
      throw new Error("user runtime lifecycle id must be a lowercase SHA-256 digest");
    }
    if (!/^[0-9a-f]{64}$/.test(intent.protectedPrestateSha256)) {
      throw new Error("user runtime protected prestate hash must be a lowercase SHA-256 digest");
    }
  }
  if (
    intent.kind === "runtime"
    && (intent.operation === "manager-deposit" || intent.operation === "manager-withdraw")
    && intent.amountRaw > binding.maxManagerOperationRaw
  ) {
    throw new Error("manager runtime amount exceeds the route policy limit");
  }
  if (
    intent.kind === "runtime"
    && (intent.operation === "manager-deposit" || intent.operation === "manager-withdraw")
  ) {
    if (!/^[0-9a-f]{64}$/.test(intent.lifecycleId)
      || !/^[0-9a-f]{64}$/.test(intent.protectedPrestateSha256)) {
      throw new Error("manager runtime lifecycle/protected-prestate binding is missing or malformed");
    }
    if (!binding.routeAuthorizationSha256 || !/^[0-9a-f]{64}$/.test(binding.routeAuthorizationSha256)) {
      throw new Error("manager runtime route-authorization binding is missing or malformed");
    }
    if (intent.routeAuthorizationSha256 !== binding.routeAuthorizationSha256) {
      throw new Error("manager runtime intent is not bound to the effective route authorization");
    }
  }
}
