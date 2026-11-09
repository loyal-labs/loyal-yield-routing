// The Loyal vault Backyard integrates: the custodial Looping vault
// (go/workers/internal/backyard/vault.go, vault "looping").
export const BACKYARD_VAULT = {
  name: "Loyal Looping",
  short: "Custodial",
  custody: "Custodial. Voltr Trustful adaptor; NAV is reported by the manager.",
  voltrVault: "GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1",
  lpMint: "5MAGdiFcSQumjZX5BDh3LqHyFog5auvEBXXJezDXAArg",
  squads: "6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY",
  navCadence:
    "at least hourly, and whenever the book moves more than 50 bps from the last report",
};

export type BackyardVault = typeof BACKYARD_VAULT;
