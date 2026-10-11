"use client";

// The component for the root layout. It renders nothing. In development it
// starts the reporter in ./browser.ts, once, in the browser; in production
// the condition below is a constant `false`, so the bundler drops the call
// and, with it, the reporter itself.
import { start } from "./browser.js";
import type { BrowserOptions } from "./browser.js";

export type LeadlineProps = BrowserOptions;

/**
 * Put `<Leadline />` in the root layout (or `pages/_app`). While you develop,
 * the page reports uncaught errors, `console.error` and `console.warn`,
 * navigations, Web Vitals and failed or slow `fetch` calls to Leadline, and
 * ties each same-origin `fetch` to the server work it caused.
 */
export function Leadline(props: LeadlineProps): null {
  if (process.env.NODE_ENV === "development" && typeof window !== "undefined") start(props);
  return null;
}
