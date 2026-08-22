import Link from "next/link";
import { connection } from "next/server";

import { BackyardVaultMonitor } from "@/features/backyard-vault-monitor";

export default async function Home() {
  await connection();
  return (
    <main className="admin-shell">
      <nav className="admin-nav" aria-label="Admin navigation">
        <Link className="brand" href="/" aria-label="Loyal admin home">
          <span className="brand-mark">L</span>
          <span>Loyal Admin</span>
        </Link>
        <span className="nav-section">Yield operations</span>
      </nav>
      <BackyardVaultMonitor />
    </main>
  );
}
