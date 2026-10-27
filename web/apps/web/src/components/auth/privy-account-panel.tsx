"use client";

import {
  getIdentityToken,
  useLinkAccount,
  useMfaEnrollment,
  usePrivy,
  useUnlinkEmail,
  useUnlinkOAuth,
} from "@privy-io/react-auth";
import { useExportWallet } from "@privy-io/react-auth/solana";
import { useUpdateEmail } from "@privy-io/react-auth/ui";
import { ShieldCheck, Wallet } from "lucide-react";
import { type ReactNode, useCallback, useEffect, useState } from "react";

import { usePrivyAuth } from "@/components/auth/privy-session-sync";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import { TextSwap } from "@/components/wallet-workspace/facelift/text-swap";
import { useAuthSession } from "@/contexts/auth-session-context";
import { usePublicEnv } from "@/contexts/public-env-context";

type PrivyAppConfig = { google_oauth?: boolean; mfa_methods?: string[] };
// Public, unauthenticated dashboard config; the SDK reads it but exposes no
// hook. ponytail: one fetch per mount, no cache — the panel is rarely open.
function usePrivyAppConfig(appId: string | undefined) {
  const [config, setConfig] = useState<PrivyAppConfig | null>(null);
  useEffect(() => {
    if (!appId) return;
    const ctrl = new AbortController();
    fetch(`https://auth.privy.io/api/v1/apps/${appId}`, {
      headers: { "privy-app-id": appId },
      signal: ctrl.signal,
    })
      .then((r) => (r.ok ? r.json() : null))
      .then((c) => c && setConfig(c as PrivyAppConfig))
      .catch(() => undefined);
    return () => ctrl.abort();
  }, [appId]);
  return config;
}

/**
 * Account panel body when Privy is on: linked email / Google, embedded-wallet
 * key export, MFA. Every Privy action runs in Privy's own modal; after a
 * link/unlink the Loyal session is re-issued so `user.email` follows.
 */
export function PrivyAccountPanel({ policyRow }: { policyRow?: ReactNode }) {
  const { user: privyUser } = usePrivy();
  const privyAuth = usePrivyAuth();
  const { user, refreshSession } = useAuthSession();
  const { privyAppId } = usePublicEnv();
  const [busy, setBusy] = useState(false);
  const appConfig = usePrivyAppConfig(privyAppId);

  const resync = useCallback(async () => {
    if (!user?.walletAddress) return;
    setBusy(true);
    try {
      const identityToken = await getIdentityToken();
      if (!identityToken) return;
      await fetch("/api/auth/privy/complete", {
        method: "POST",
        credentials: "include",
        headers: {
          "content-type": "application/json",
          "privy-id-token": identityToken,
        },
        body: JSON.stringify({ walletAddress: user.walletAddress }),
      });
      await refreshSession();
    } finally {
      setBusy(false);
    }
  }, [refreshSession, user?.walletAddress]);

  const { linkEmail, linkGoogle } = useLinkAccount({ onSuccess: resync });
  const { update: updateEmail } = useUpdateEmail();
  const { unlink: unlinkEmail } = useUnlinkEmail();
  const { unlink: unlinkOAuth } = useUnlinkOAuth();
  const { exportWallet } = useExportWallet();
  const { showMfaEnrollmentModal } = useMfaEnrollment();

  if (!privyUser) {
    return (
      <div className="flex flex-col">
        <Row
          action="Add"
          disabled={!privyAuth || privyAuth.step !== "idle"}
          icon={
            <ThemedIcon
              className="size-6"
              src="/wallet-workspace/facelift/settings/email.svg"
            />
          }
          onClick={() => privyAuth?.addEmail()}
          subtitle="For account updates and recovery."
          title="Email"
        />
        {privyAuth?.error ? (
          <p className="px-4 text-[13px] text-destructive leading-4">
            {privyAuth.error}
          </p>
        ) : null}
        {policyRow}
      </div>
    );
  }
  const linked = privyUser.linkedAccounts;
  const email = linked.find((a) => a.type === "email");
  const google = linked.find((a) => a.type === "google_oauth");
  const embedded = linked.find(
    (a): a is Extract<typeof a, { type: "wallet" }> =>
      a.type === "wallet" &&
      a.chainType === "solana" &&
      a.walletClientType === "privy"
  );
  // Privy refuses to unlink the last linked account; mirror that in the UI.
  const canUnlink = linked.length > 1;
  const mfaOn = privyUser.mfaMethods.length > 0;
  const googleEnabled = appConfig?.google_oauth === true;
  const mfaEnabled = (appConfig?.mfa_methods?.length ?? 0) > 0;

  return (
    <div className="flex flex-col">
      {google || googleEnabled ? (
        <Row
          action={google ? (canUnlink ? "Unlink" : undefined) : "Link"}
          disabled={busy}
          icon={
            <ThemedIcon
              className="size-6"
              src="/wallet-workspace/facelift/settings/google.svg"
            />
          }
          onClick={() =>
            google
              ? canUnlink
                ? unlinkOAuth({
                    provider: "google",
                    subject: google.subject,
                  }).then(resync)
                : undefined
              : linkGoogle()
          }
          subtitle={google ? google.email ?? "Linked" : "Sign in with Google."}
          title="Google"
        />
      ) : null}
      <Row
        action={email ? (canUnlink ? "Unlink" : "Change") : "Add"}
        disabled={busy}
        icon={
          <ThemedIcon
            className="size-6"
            src="/wallet-workspace/facelift/settings/email.svg"
          />
        }
        onClick={() =>
          email
            ? canUnlink
              ? unlinkEmail({ address: email.address }).then(resync)
              : updateEmail()
            : linkEmail()
        }
        subtitle={email ? email.address : "For account updates and recovery."}
        title="Email"
      />
      {policyRow}
      {embedded ? (
        <Row
          action="chevron"
          disabled={busy}
          icon={
            <ThemedIcon
              className="size-6"
              src="/wallet-workspace/facelift/settings/key.svg"
            />
          }
          onClick={() => exportWallet({ address: embedded.address })}
          subtitle=""
          title="Export Keys"
        />
      ) : (
        <Row
          disabled
          icon={<Wallet className="size-6" />}
          subtitle="Signed in with your own wallet."
          title="Wallet"
        />
      )}
      {mfaEnabled || mfaOn ? (
        <Row
          action={mfaOn ? "Manage" : "Enable"}
          disabled={busy}
          icon={<ShieldCheck className="size-6" />}
          onClick={showMfaEnrollmentModal}
          subtitle={
            mfaOn
              ? "On — extra check before signing."
              : "Extra check before signing."
          }
          title="Two-factor"
        />
      ) : null}
    </div>
  );
}

function Row({
  action,
  disabled,
  icon,
  onClick,
  subtitle,
  title,
}: {
  action?: string;
  disabled?: boolean;
  icon: ReactNode;
  onClick?: () => unknown;
  subtitle: string;
  title: string;
}) {
  const interactive = Boolean(onClick && action);
  // Same row recipe as the sidebar links: t-hover + hover:bg-accent, 44px
  // icon slot, tertiary icon. Static rows keep the layout but no hover.
  return (
    <button
      className="t-hover flex min-h-[60px] w-full items-center rounded-2xl px-4 text-left enabled:hover:bg-accent disabled:cursor-default"
      disabled={disabled || !interactive}
      onClick={() => void onClick?.()}
      type="button"
    >
      <span className="mr-4 flex h-12 w-6 shrink-0 items-center justify-center text-tertiary">
        {icon}
      </span>
      <span className="min-w-0 flex-1 py-[11px]">
        <span className="block font-medium text-[16px] text-foreground leading-5">
          {title}
        </span>
        <span className="mt-0.5 block truncate text-muted-foreground text-[13px] leading-4">
          {subtitle}
        </span>
      </span>
      {action === "chevron" ? (
        <ThemedIcon
          className="ml-1.5 size-6 text-tertiary"
          src="/wallet-workspace/facelift/icon-chevron-right.svg"
        />
      ) : action ? (
        <span className="ml-3 shrink-0 rounded-full bg-accent px-4 py-2.5 text-center font-medium text-[13px] text-foreground leading-4">
          <TextSwap text={action} />
        </span>
      ) : null}
    </button>
  );
}
