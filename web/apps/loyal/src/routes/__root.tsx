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
const publicEnv = createPublicEnv(process.env);

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
  notFoundComponent: NotFound,
  errorComponent: ({ error, reset }) => (
    <AppError error={error instanceof Error ? error : new Error(String(error))} reset={reset} />
  ),
});
