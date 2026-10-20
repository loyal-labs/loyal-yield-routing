import "@/app/globals.css";

import { createRootRoute, HeadContent, Outlet } from "@tanstack/react-router";

import AppError from "@/app/error";
import { metadata } from "@/app/layout";
import NotFound from "@/app/not-found";
import { XPixelBootstrap } from "@/components/analytics/x-pixel-bootstrap";
import { AppTransitionBridge } from "@/components/app-transition/app-transition-bridge";
import { PublicEnvProvider } from "@/contexts/public-env-context";
import { createPublicEnv } from "@/lib/core/config/public";

import { headFromMetadata } from "../next-compat/metadata";

// apps/web src/app/layout.tsx, minus <html>/<body> (index.html) and the
// server-side env read: process.env here is the NEXT_PUBLIC_* set Vite inlines.
// Captcha is the one field layout.tsx derives from a server secret (CAP_SECRET);
// the server that verifies it is the forwarded /api upstream, so a deployed
// build always shows the widget.
const resolvedEnv = createPublicEnv(process.env);
const publicEnv =
  resolvedEnv.captcha.mode === "misconfigured"
    ? { ...resolvedEnv, captcha: { mode: "widget" as const } }
    : resolvedEnv;

export const Route = createRootRoute({
  head: () => headFromMetadata(metadata),
  component: () => (
    <>
      <HeadContent />
      <PublicEnvProvider value={publicEnv}>
        <XPixelBootstrap />
        <AppTransitionBridge />
        <Outlet />
      </PublicEnvProvider>
    </>
  ),
  // NotFound's Footer reads the public env; the root notFoundComponent renders
  // outside the component tree above.
  notFoundComponent: () => (
    <PublicEnvProvider value={publicEnv}>
      <NotFound />
    </PublicEnvProvider>
  ),
  errorComponent: ({ error, reset }) => (
    <AppError error={error instanceof Error ? error : new Error(String(error))} reset={reset} />
  ),
});
