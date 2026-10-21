import { useRouter as useTanstackRouter } from "@tanstack/react-router";
import type { AnchorHTMLAttributes, MouseEvent, ReactNode } from "react";

export type LinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "href"> & {
  href: string | { pathname?: string; query?: Record<string, string>; hash?: string };
  replace?: boolean;
  scroll?: boolean;
  prefetch?: boolean | null;
  children?: ReactNode;
};

export type HrefKind = "internal" | "external";

export function classifyHref(href: LinkProps["href"], origin: string): { kind: HrefKind; url: string } {
  const url = typeof href === "string" ? href : stringifyHrefObject(href);
  if (url === "" || url.startsWith("#")) return { kind: "external", url };
  const absolute = new URL(url, origin);
  if (absolute.origin !== origin) return { kind: "external", url };
  // ?lang= is the language switch's request to the locale middleware (now
  // go/workers/internal/webapp/locale.go) to remember a choice; Next runs
  // middleware on Link navigations, here only a page load reaches it.
  if (absolute.searchParams.has("lang")) return { kind: "external", url };
  return { kind: "internal", url: `${absolute.pathname}${absolute.search}${absolute.hash}` };
}

function stringifyHrefObject(href: Exclude<LinkProps["href"], string>): string {
  const query = href.query ? `?${new URLSearchParams(href.query)}` : "";
  const hash = href.hash ? (href.hash.startsWith("#") ? href.hash : `#${href.hash}`) : "";
  return `${href.pathname ?? ""}${query}${hash}`;
}

const MODIFIER_KEYS = ["metaKey", "ctrlKey", "shiftKey", "altKey"] as const;

export default function Link({ href, replace, scroll, prefetch, onClick, onMouseEnter, onFocus, ...rest }: LinkProps) {
  const router = useTanstackRouter();
  const { kind, url } = classifyHref(href, window.location.origin);
  const navigates = kind === "internal" && !rest.target && !rest.download;

  const handleClick = (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event);
    if (!navigates || event.defaultPrevented) return;
    if (event.button !== 0 || MODIFIER_KEYS.some((key) => event[key])) return;
    event.preventDefault();
    // TTo pinned to string: href carries the fully built path, `to` stays
    // unset as the types then allow.
    router
      .navigate<typeof router, string>({ href: url, replace, resetScroll: scroll === false ? false : undefined })
      .catch(() => {});
  };

  if (!navigates) return <a href={url} onClick={onClick} onMouseEnter={onMouseEnter} onFocus={onFocus} {...rest} />;

  const preload = () => {
    if (prefetch === false) return;
    router.preloadRoute<string, string>({ href: url }).catch(() => {});
  };

  return (
    <a
      href={url}
      onClick={handleClick}
      onMouseEnter={(event) => {
        onMouseEnter?.(event);
        preload();
      }}
      onFocus={(event) => {
        onFocus?.(event);
        preload();
      }}
      {...rest}
    />
  );
}
