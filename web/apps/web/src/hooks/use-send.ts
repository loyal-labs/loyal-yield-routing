import type { AnalyticsProperties } from "@loyal-labs/shared/analytics";
import { TOKEN_DECIMALS, TOKEN_MINTS } from "@loyal-labs/wallet-core/constants";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import {
  createAssociatedTokenAccountInstruction,
  createTransferInstruction,
  getAccount,
  getAssociatedTokenAddress,
} from "@solana/spl-token";
import {
  ComputeBudgetProgram,
  LAMPORTS_PER_SOL,
  PublicKey,
  SystemProgram,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import { useCallback, useState } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";
import { trackWalletSendCompleted } from "@/lib/core/analytics";
import {
  type SentTransaction,
  sendRecoveringSignature,
  settleSentTransaction,
  transactionFailedMessage,
  type TransactionStatusUnknown,
  transactionStatusUnknown,
} from "@/lib/solana/confirm-sent-transaction";

function cleanSolanaErrorMessage(message: string): string {
  const logsIndex = message.indexOf("Logs:");
  if (logsIndex !== -1) {
    return message.slice(0, logsIndex).trim();
  }
  return message;
}

export type SendResult =
  | { signature: string; success: true }
  | { error: string; success: false; transactionWasSubmitted?: false }
  | TransactionStatusUnknown;

const getTokenMint = (symbol: string): string | undefined => {
  const normalizedSymbol = symbol.toUpperCase();
  return TOKEN_MINTS[normalizedSymbol];
};

export function useSend() {
  const { connection } = useConnection();
  const {
    publicKey: walletPublicKey,
    connected: isConnected,
    sendTransaction,
  } = useWallet();
  const publicEnv = usePublicEnv();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const executeSend = useCallback(
    async (
      currency: string,
      amount: string,
      recipientAddress: string,
      tokenMint?: string,
      tokenDecimals?: number,
      successTrackingProperties?: AnalyticsProperties
    ): Promise<SendResult> => {
      if (!(isConnected && walletPublicKey)) {
        const error = "Wallet not connected";
        setError(error);
        return { success: false, error };
      }

      setLoading(true);
      setError(null);

      const broadcast = async (): Promise<SentTransaction> => {
        const publicKey = walletPublicKey;
        const isSol = currency.toUpperCase() === "SOL";

        let recipientPubkey: PublicKey;
        try {
          recipientPubkey = new PublicKey(recipientAddress);
        } catch {
          throw new Error("Invalid recipient wallet address");
        }

        const { blockhash, lastValidBlockHeight } =
          await connection.getLatestBlockhash();

        if (isSol) {
          const amountInLamports = Math.floor(
            Number.parseFloat(amount) * LAMPORTS_PER_SOL
          );

          const transferInstruction = SystemProgram.transfer({
            fromPubkey: publicKey,
            toPubkey: recipientPubkey,
            lamports: amountInLamports,
          });

          const messageV0 = new TransactionMessage({
            payerKey: publicKey,
            recentBlockhash: blockhash,
            instructions: [transferInstruction],
          }).compileToV0Message();

          const transaction = new VersionedTransaction(messageV0);

          return {
            lastValidBlockHeight,
            signature: await sendRecoveringSignature(() =>
              sendTransaction(transaction, connection)
            ),
          };
        }
        const resolvedTokenMint = tokenMint || getTokenMint(currency);
        if (!resolvedTokenMint) {
          throw new Error(
            `Unknown token: ${currency}. Please provide token mint address.`
          );
        }

        const mintPubkey = new PublicKey(resolvedTokenMint);

        const decimals =
          tokenDecimals ?? TOKEN_DECIMALS[currency.toUpperCase()] ?? 6;
        const amountInSmallestUnit = Math.floor(
          Number.parseFloat(amount) * 10 ** decimals
        );

        // Get associated token accounts.
        // Recipient may be a PDA (e.g. a smart-account vault), which is off
        // the ed25519 curve. Without allowOwnerOffCurve=true the SPL helper
        // throws TokenOwnerOffCurveError before we can build a transfer.
        const fromTokenAccount = await getAssociatedTokenAddress(
          mintPubkey,
          publicKey
        );

        const toTokenAccount = await getAssociatedTokenAddress(
          mintPubkey,
          recipientPubkey,
          true
        );

        let needsATA = false;

        try {
          await getAccount(connection, toTokenAccount);
        } catch {
          needsATA = true;
        }

        const instructions = [];

        if (needsATA) {
          instructions.push(
            ComputeBudgetProgram.setComputeUnitLimit({
              units: 300_000,
            })
          );
          instructions.push(
            ComputeBudgetProgram.setComputeUnitPrice({
              microLamports: 1000,
            })
          );

          instructions.push(
            createAssociatedTokenAccountInstruction(
              publicKey,
              toTokenAccount, // ata
              recipientPubkey, // owner
              mintPubkey // mint
            )
          );
        }

        instructions.push(
          createTransferInstruction(
            fromTokenAccount,
            toTokenAccount,
            publicKey,
            amountInSmallestUnit
          )
        );

        const messageV0 = new TransactionMessage({
          payerKey: publicKey,
          recentBlockhash: blockhash,
          instructions,
        }).compileToV0Message();

        const transaction = new VersionedTransaction(messageV0);

        return {
          lastValidBlockHeight,
          signature: await sendRecoveringSignature(() =>
            sendTransaction(transaction, connection)
          ),
        };
      };

      let sent: SentTransaction;
      try {
        sent = await broadcast();
      } catch (err) {
        let errorMessage = "Send execution failed";

        if (err instanceof Error) {
          if (
            err.message.includes("timeout") ||
            err.message.includes("Timeout")
          ) {
            errorMessage =
              "Transaction signing timed out. Please try again and approve the transaction in your wallet promptly.";
          } else if (err.message.includes("User rejected")) {
            errorMessage = "Transaction was rejected in your wallet.";
          } else {
            errorMessage = cleanSolanaErrorMessage(err.message);
          }
        }

        setError(errorMessage);
        console.error("Send execution error:", err);
        setLoading(false);
        return { success: false, error: errorMessage };
      }

      const outcome = await settleSentTransaction(connection, sent);
      setLoading(false);
      if (outcome.kind === "failed") {
        const errorMessage = transactionFailedMessage(outcome.err);
        setError(errorMessage);
        return { success: false, error: errorMessage };
      }
      if (outcome.kind === "status-unknown") {
        const unknown = transactionStatusUnknown(sent.signature);
        setError(unknown.error);
        return unknown;
      }
      if (successTrackingProperties) {
        trackWalletSendCompleted(publicEnv, {
          ...successTrackingProperties,
          signature: sent.signature,
        });
      }
      return { signature: sent.signature, success: true };
    },
    [isConnected, walletPublicKey, sendTransaction, connection, publicEnv]
  );

  return {
    executeSend,
    loading,
    error,
  };
}
