import "server-only";

export type QuestKind = "earn_deposit" | "first_autodeposit_sweep";

export type QuestCompletionResult =
  | { status: "completed" | "already_completed" }
  | { status: "disabled"; reason: string }
  | { status: "skipped"; reason: string }
  | {
      status: "permanent_error";
      httpStatus: number;
      error: string;
      message: string;
    }
  | { status: "retryable_error"; httpStatus?: number; error: string };

export type QuestCompletionMetadata = Record<string, unknown>;

export async function reportQuestCompletion(): Promise<QuestCompletionResult> {
  return {
    status: "disabled",
    reason: "Seeker Summer round 1 ended — Solana-side reporting retired",
  };
}
