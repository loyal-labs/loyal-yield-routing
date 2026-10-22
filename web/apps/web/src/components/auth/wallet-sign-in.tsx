"use client";

import { useWallet } from "@solana/wallet-adapter-react";
import { useEffect, useState } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";

import { CapWidget } from "./cap-widget";
import { WalletTab } from "./wallet-tab";

export function WalletSignIn({
  defaultUseLedgerProof = false,
}: {
  defaultUseLedgerProof?: boolean;
} = {}) {
  const { captcha } = usePublicEnv();
  const [captchaToken, setCaptchaToken] = useState<string | null>(null);
  const captchaSatisfied = captcha.mode === "disabled" || captchaToken !== null;
  // No installed wallet extension and no connected wallet means sign-in
  // cannot proceed, so asking for captcha verification is pointless noise
  // (ASK-2066). Mirrors the `readyState === "Installed"` filter in
  // use-wallet-proof-auth.
  const { connected, wallets } = useWallet();
  const hasWalletOption =
    connected ||
    wallets.some((candidate) => candidate.readyState === "Installed");

  const [isCollapsed, setIsCollapsed] = useState(false);
  useEffect(() => {
    if (captchaToken === null) {
      setIsCollapsed(false);
      return;
    }
    const timer = setTimeout(() => setIsCollapsed(true), 600);
    return () => clearTimeout(timer);
  }, [captchaToken]);

  const [widgetEpoch, setWidgetEpoch] = useState(0);

  return (
    <div className="flex flex-col">
      {hasWalletOption && captcha.mode !== "disabled" ? (
        <div className="t-acc" data-open={isCollapsed ? "false" : "true"}>
          <div className="t-acc-panel">
            <div className="t-acc-panel-inner">
              <div className="flex flex-col gap-3 pb-4">
                <p className="text-muted-foreground text-sm">
                  Complete verification to continue
                </p>
                <CapWidget key={widgetEpoch} onVerify={setCaptchaToken} />
              </div>
            </div>
          </div>
        </div>
      ) : null}
      <p className="pb-4 text-muted-foreground text-sm">
        Choose your preferred sign-in method.
      </p>
      <WalletTab
        captchaSatisfied={captchaSatisfied}
        captchaToken={captchaToken}
        defaultUseLedgerProof={defaultUseLedgerProof}
        onCaptchaConsumed={() => {
          setCaptchaToken(null);
          setWidgetEpoch((epoch) => epoch + 1);
        }}
      />
    </div>
  );
}
