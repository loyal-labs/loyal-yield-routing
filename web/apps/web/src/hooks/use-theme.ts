"use client";

import { useCallback, useEffect, useState } from "react";

import { THEME_STORAGE_KEY } from "@/lib/theme";

const WIPE_DURATION_MS = 400;
const WIPE_EASE = "cubic-bezier(0.23, 1, 0.32, 1)";

type ViewTransitionDocument = Document & {
  startViewTransition?: (update: () => void) => {
    finished: Promise<void>;
    ready: Promise<void>;
  };
};

export function useTheme() {
  const [isDark, setIsDark] = useState(false);

  useEffect(() => {
    setIsDark(document.documentElement.classList.contains("dark"));
  }, []);

  const toggleTheme = useCallback((origin?: { x: number; y: number }) => {
    const root = document.documentElement;
    const next = !root.classList.contains("dark");
    const apply = () => {
      root.classList.toggle("dark", next);
      try {
        localStorage.setItem(THEME_STORAGE_KEY, next ? "dark" : "light");
      } catch {
      }
      setIsDark(next);
    };

    const doc = document as ViewTransitionDocument;
    if (!doc.startViewTransition) {
      apply();
      return;
    }
    const wantsWipe =
      origin !== undefined &&
      !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (!wantsWipe) {
      doc.startViewTransition(apply);
      return;
    }

    // The attribute scopes the snapshot freeze in globals.css to this wipe,
    // so other view transitions keep their default crossfade. A rapid
    // re-toggle can drop the freeze early (the first transition's cleanup
    // races the second's run) — the fallback is just a crossfade, fine.
    root.setAttribute("data-theme-wipe", "");
    const transition = doc.startViewTransition(apply);
    transition.ready
      .then(() => {
        const radius = Math.hypot(
          Math.max(origin.x, window.innerWidth - origin.x),
          Math.max(origin.y, window.innerHeight - origin.y)
        );
        root.animate(
          {
            clipPath: [
              `circle(0px at ${origin.x}px ${origin.y}px)`,
              `circle(${radius}px at ${origin.x}px ${origin.y}px)`,
            ],
          },
          {
            duration: WIPE_DURATION_MS,
            easing: WIPE_EASE,
            pseudoElement: "::view-transition-new(root)",
          }
        );
      })
      .catch(() => {
      });
    void transition.finished.finally(() => {
      root.removeAttribute("data-theme-wipe");
    });
  }, []);

  return { isDark, toggleTheme };
}
