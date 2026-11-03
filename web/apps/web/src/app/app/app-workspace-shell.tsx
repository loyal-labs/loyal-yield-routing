"use client";

import { AutodepositMockSheet } from "@/components/wallet-sidebar/autodeposit-mock-sheet";
import { WorkspaceFaceliftShell } from "@/components/wallet-workspace/facelift/shell";
import { useClaimPendingCode } from "@/features/referrals/use-claim-pending-code";

export function AppWorkspaceShell() {
  useClaimPendingCode();
  return (
    <>
      <WorkspaceFaceliftShell />
      <AutodepositMockSheet />
    </>
  );
}
