"use client";

import type { ReactNode } from "react";

import {
  ActionErrorBody,
  ActionProcessingBody,
  ActionSuccessBody,
} from "@/components/wallet-workspace/facelift/action-screens";
import { usePublicEnv } from "@/contexts/public-env-context";
import { openTrackedLink } from "@/lib/core/analytics";

const relayExplorer = (requestId: string) =>
  `https://relay.link/transaction/${requestId}`;

function ExplorerButton({ href, source }: { href: string; source: string }) {
  const publicEnv = usePublicEnv();
  return (
    <button
      className="t-hover flex h-12 items-center justify-center rounded-full bg-accent px-5 font-medium text-[16px] text-foreground leading-5 hover:bg-accent-active"
      onClick={() =>
        openTrackedLink(publicEnv, {
          href,
          linkText: "View in Explorer",
          source,
        })
      }
      type="button"
    >
      View in Explorer
    </button>
  );
}

export type BridgeStatus =
  | { kind: "bridging"; icons: ReactNode; requestId: string }
  | { kind: "filled"; signature: string | null; explorerHref?: string }
  | { kind: "failed"; message: string; requestId: string | null };

export function BridgeStatusBody({
  status,
  source,
  onBack,
}: {
  status: BridgeStatus;
  source: string;
  onBack: () => void;
}) {
  if (status.kind === "bridging")
    return (
      <ActionProcessingBody icons={status.icons} label="Bridging...">
        <div className="flex flex-col items-center gap-3">
          <ExplorerButton
            href={relayExplorer(status.requestId)}
            source={`${source}_processing`}
          />
          <button
            className="t-hover flex h-12 items-center justify-center rounded-full bg-foreground px-5 font-medium text-[16px] text-background leading-5 hover:-translate-y-0.5 hover:bg-foreground/90 active:translate-y-0"
            onClick={onBack}
            type="button"
          >
            Done
          </button>
        </div>
      </ActionProcessingBody>
    );
  if (status.kind === "filled")
    return (
      <ActionSuccessBody
        explorerHref={status.explorerHref}
        label="Swapped"
        onDone={onBack}
        signature={status.signature}
        source={`${source}_success`}
      />
    );
  return (
    <ActionErrorBody
      backLabel="Done"
      explorer={
        status.requestId ? (
          <ExplorerButton
            href={relayExplorer(status.requestId)}
            source={`${source}_failed`}
          />
        ) : null
      }
      message={status.message}
      onBack={onBack}
    />
  );
}
