import { useEffect, useState } from "react";

/** Every chart with time across shares this key, so hovering one marks the same moment on all of them. */
export const TIME_SYNC = "leadline-time";

/** useScheme changes when the system switches between light and dark. */
export function useScheme(): boolean {
  const [dark, setDark] = useState(() => matchMedia("(prefers-color-scheme: dark)").matches);
  useEffect(() => {
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const on = () => setDark(mq.matches);
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return dark;
}

/**
 * tokens reads colour tokens (styles.css) as the canvas needs them: plain
 * six-digit hex. A chart adds alpha by appending two more digits.
 */
export function tokens(el: Element): (name: string) => string {
  const css = getComputedStyle(el);
  return (name) => css.getPropertyValue(name).trim();
}

/** alpha writes 0..1 as the two hex digits that follow a six-digit colour. */
export function alpha(a: number): string {
  return Math.round(Math.min(1, Math.max(0, a)) * 255)
    .toString(16)
    .padStart(2, "0");
}

/** axisFont is the one font every chart's axes use. */
export function axisFont(token: (name: string) => string): string {
  return `11px ${token("--font-sans") || "sans-serif"}`;
}

/**
 * heat is how strongly a count shows against the fullest cell. Counts are
 * spread on a log scale, and the floor keeps a lone slow request (one in a
 * cell, where the fast band holds hundreds) clearly on the chart.
 */
export function heat(n: number, max: number): number {
  if (n <= 0 || max <= 0) return 0;
  return 0.2 + 0.8 * (Math.log1p(n) / Math.log1p(Math.max(max, n)));
}
