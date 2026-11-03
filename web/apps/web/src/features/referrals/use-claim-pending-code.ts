"use client";

import { useEffect, useRef } from "react";

import { earnToast } from "@/components/wallet-workspace/facelift/earn-toast";
import { useAuthSession } from "@/contexts/auth-session-context";

import { claimCode } from "./claim";
import { failureCopy, referralCopy } from "./copy";
import { clearPendingCode, readPendingCode } from "./pending-code";

export function useClaimPendingCode(): void {
  const { isAuthenticated, user } = useAuthSession();
  const subject = isAuthenticated ? user?.subjectAddress : undefined;
  const claimedFor = useRef<string | null>(null);

  useEffect(() => {
    if (!subject || claimedFor.current === subject) return;
    claimedFor.current = subject;
    const pending = readPendingCode();
    if (!pending) return;
    void claimCode(pending).then((outcome) => {
      if (outcome.kind === "failed" && !outcome.answered) return;
      clearPendingCode();
      if (outcome.kind === "bound") earnToast.success(referralCopy.joined);
      else if (pending.source === "typed") {
        earnToast.error(failureCopy[outcome.failure]);
      }
    });
  }, [subject]);
}
