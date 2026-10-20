// Every page and layout of apps/web src/app, and the route that renders it
// here. The root layout is src/routes/__root.tsx. scripts/web/gen-routes.ts
// scaffolds a missing route file from a row and, with --check, fails when a
// row has no route file or a Next page has no row.

export type Owner = "G2" | "G3" | "G4";

export type RouteRow = {
  next: string; // under apps/web/src/app
  path: string; // TanStack path
  kind: "page" | "layout";
  host: "app" | "site";
  owner: Owner;
};

export const routes: RouteRow[] = [
  { next: "page.tsx", path: "/", kind: "page", host: "site", owner: "G3" },
  { next: "500/page.tsx", path: "/500", kind: "page", host: "site", owner: "G3" },
  { next: "agents/page.tsx", path: "/agents", kind: "page", host: "site", owner: "G3" },
  { next: "earn/page.tsx", path: "/earn", kind: "page", host: "site", owner: "G3" },
  { next: "landing/layout.tsx", path: "/landing", kind: "layout", host: "site", owner: "G3" },
  { next: "landing/page.tsx", path: "/landing", kind: "page", host: "site", owner: "G3" },
  { next: "privacy-policy/page.tsx", path: "/privacy-policy", kind: "page", host: "site", owner: "G3" },
  { next: "risks/page.tsx", path: "/risks", kind: "page", host: "site", owner: "G3" },
  { next: "trust/page.tsx", path: "/trust", kind: "page", host: "site", owner: "G3" },
  { next: "ru/layout.tsx", path: "/ru", kind: "layout", host: "site", owner: "G3" },
  { next: "ru/page.tsx", path: "/ru", kind: "page", host: "site", owner: "G3" },
  { next: "ru/agents/page.tsx", path: "/ru/agents", kind: "page", host: "site", owner: "G3" },
  { next: "ru/earn/page.tsx", path: "/ru/earn", kind: "page", host: "site", owner: "G3" },
  { next: "ru/risks/page.tsx", path: "/ru/risks", kind: "page", host: "site", owner: "G3" },
  { next: "ru/trust/page.tsx", path: "/ru/trust", kind: "page", host: "site", owner: "G3" },
  { next: "zh/layout.tsx", path: "/zh", kind: "layout", host: "site", owner: "G3" },
  { next: "zh/page.tsx", path: "/zh", kind: "page", host: "site", owner: "G3" },
  { next: "zh/agents/page.tsx", path: "/zh/agents", kind: "page", host: "site", owner: "G3" },
  { next: "zh/earn/page.tsx", path: "/zh/earn", kind: "page", host: "site", owner: "G3" },
  { next: "zh/risks/page.tsx", path: "/zh/risks", kind: "page", host: "site", owner: "G3" },
  { next: "zh/trust/page.tsx", path: "/zh/trust", kind: "page", host: "site", owner: "G3" },
  { next: "blog/page.tsx", path: "/blog", kind: "page", host: "site", owner: "G4" },
  { next: "blog/[slug]/page.tsx", path: "/blog/$slug", kind: "page", host: "site", owner: "G4" },
  { next: "app/layout.tsx", path: "/app", kind: "layout", host: "app", owner: "G2" },
  { next: "app/page.tsx", path: "/app", kind: "page", host: "app", owner: "G2" },
  { next: "app/policies/page.tsx", path: "/app/policies", kind: "page", host: "app", owner: "G2" },
  { next: "app/settings/page.tsx", path: "/app/settings", kind: "page", host: "app", owner: "G2" },
  { next: "app/cherry/layout.tsx", path: "/app/cherry", kind: "layout", host: "app", owner: "G2" },
  { next: "app/cherry/page.tsx", path: "/app/cherry", kind: "page", host: "app", owner: "G2" },
  { next: "dev/txv1/page.tsx", path: "/dev/txv1", kind: "page", host: "app", owner: "G2" },
];
