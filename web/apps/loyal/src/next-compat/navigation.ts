import { notFound as routerNotFound, useLocation, useRouter as useTanstackRouter } from "@tanstack/react-router";

export type AppRouter = {
  push(href: string, options?: { scroll?: boolean }): void;
  replace(href: string, options?: { scroll?: boolean }): void;
  back(): void;
  forward(): void;
  refresh(): void;
  prefetch(href: string): void;
};

export function usePathname(): string {
  return useLocation().pathname;
}

export function useSearchParams(): URLSearchParams {
  return new URLSearchParams(useLocation().searchStr);
}

// Seed body: history-level navigation. G1 decides preload and refresh semantics.
export function useRouter(): AppRouter {
  const router = useTanstackRouter();
  return {
    push: (href) => router.history.push(href),
    replace: (href) => router.history.replace(href),
    back: () => router.history.back(),
    forward: () => router.history.forward(),
    refresh: () => void router.invalidate(),
    prefetch: () => undefined,
  };
}

export function notFound(): never {
  throw routerNotFound();
}

export function redirect(href: string): never {
  window.location.assign(href);
  throw new Error(`redirect to ${href}`);
}
