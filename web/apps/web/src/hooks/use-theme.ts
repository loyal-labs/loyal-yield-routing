"use client";

import { useCallback, useEffect, useState } from "react";

const WIPE_DURATION_MS = 400;
const WIPE_EASE = "cubic-bezier(0.23, 1, 0.32, 1)";

type ViewTransitionDocument = Document & {
  startViewTransition?: (update: () => void) => {
    finished: Promise<void>;
    ready: Promise<void>;
  };
};

// The pre-paint script in app/layout.tsx owns initial theme resolution
// (light for everyone unless localStorage "theme" is "dark" — system
// preference is deliberately ignored); this hook only mirrors and mutates
// the html.dark class it set.
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
        localStorage.setItem("theme", next ? "dark" : "light");
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
