export type OnboardingSlide = {
  title: string;
  description: string;
  image: number;
};

export type WalletSetupAction = {
  id: "connect-wallet" | "create" | "login" | "import";
  label: string;
  helperText?: string;
};

/**
 * Which external-wallet connect path this binary supports: "mwa" on builds
 * with the MWA native module, "seed-vault" as the legacy fallback on older
 * Seeker builds receiving this bundle via OTA, "deeplink" on iOS with
 * Phantom/Solflare installed, "none" elsewhere.
 */
export type WalletConnectMode = "mwa" | "seed-vault" | "deeplink" | "none";

export type OnboardingMode = "setup" | "replay";

export type OnboardingStartStep = "slides" | "setup-onboarding";

export const ONBOARDING_SLIDES: OnboardingSlide[] = [
  {
    title: "Autodeposit",
    description:
      "Connect your wallet once and earn the best rate on USDC with loyal automations",
    image: require("../../../assets/images/onboarding/autodeposit.png"),
  },
];

export function buildWalletSetupActions(
  connectMode: WalletConnectMode,
): WalletSetupAction[] {
  const createAndImport: WalletSetupAction[] = [
    { id: "create", label: "Create New Wallet" },
    { id: "login", label: "Log In with Email" },
    { id: "import", label: "Import Existing Wallet" },
  ];

  if (connectMode === "none") return createAndImport;

  return [
    connectMode === "seed-vault"
      ? { id: "connect-wallet", label: "Use Seed Vault" }
      : {
          id: "connect-wallet",
          label: "Connect Wallet",
          helperText:
            connectMode === "deeplink"
              ? "Phantom or Solflare"
              : "Phantom, Solflare, or Seed Vault",
        },
    ...createAndImport,
  ];
}

export function getSetupStartStep(mode: OnboardingMode): OnboardingStartStep {
  return mode === "setup" ? "setup-onboarding" : "slides";
}
