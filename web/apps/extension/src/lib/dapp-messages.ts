import type { Base64 } from "./brands";

export interface DappConnectRequest {
  type: "DAPP_CONNECT_REQUEST";
  id: string;
  origin: string;
  favicon?: string;
}

export interface DappConnectResponse {
  type: "DAPP_CONNECT_RESPONSE";
  id: string;
  approved: boolean;
  publicKey?: string; // base58
  error?: string;
}

export interface DappSignTransactionRequest {
  type: "DAPP_SIGN_TRANSACTION_REQUEST";
  id: string;
  origin: string;
  favicon?: string;
  transaction: Base64;
}

export interface DappSignTransactionResponse {
  type: "DAPP_SIGN_TRANSACTION_RESPONSE";
  id: string;
  approved: boolean;
  signedTransaction?: Base64;
  error?: string;
}

export interface DappSignMessageRequest {
  type: "DAPP_SIGN_MESSAGE_REQUEST";
  id: string;
  origin: string;
  favicon?: string;
  message: Base64;
}

export interface DappSignMessageResponse {
  type: "DAPP_SIGN_MESSAGE_RESPONSE";
  id: string;
  approved: boolean;
  signature?: Base64;
  error?: string;
}

export interface DappDisconnect {
  type: "DAPP_DISCONNECT";
  origin: string;
}

export type DappToBackgroundMessage =
  | DappConnectRequest
  | DappSignTransactionRequest
  | DappSignMessageRequest
  | DappDisconnect;

export type BackgroundToDappResponse =
  | DappConnectResponse
  | DappSignTransactionResponse
  | DappSignMessageResponse;

export interface DappApprovalDecision {
  type: "DAPP_APPROVAL_DECISION";
  id: string;
  nonce: string;
  approved: boolean;
}
