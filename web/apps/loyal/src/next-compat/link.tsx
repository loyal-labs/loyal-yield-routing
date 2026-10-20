import type { AnchorHTMLAttributes, ReactNode } from "react";

export type LinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "href"> & {
  href: string | { pathname?: string; query?: Record<string, string>; hash?: string };
  replace?: boolean;
  scroll?: boolean;
  prefetch?: boolean | null;
  children?: ReactNode;
};

// Seed body: a plain anchor (full page loads). G1 routes in-app hrefs through
// TanStack Router.
export default function Link({ href, replace: _replace, scroll: _scroll, prefetch: _prefetch, ...rest }: LinkProps) {
  const url = typeof href === "string" ? href : `${href.pathname ?? ""}${href.query ? `?${new URLSearchParams(href.query)}` : ""}${href.hash ?? ""}`;
  return <a href={url} {...rest} />;
}
