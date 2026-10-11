import type { KeyboardEvent } from "react";

const TABBABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]';

/**
 * keepTab keeps Tab inside the element it is put on: from its last control
 * Tab goes to its first, and Shift+Tab the other way. It is for a dialog's
 * popup. The dialog already brings focus in and gives it back on closing,
 * but at the end of a round it lets Tab out onto the page behind (in the
 * desktop app's window there is no browser chrome to catch it), where
 * Escape and the dialog's own keys no longer reach it.
 */
export function keepTab(e: KeyboardEvent<HTMLElement>): void {
  if (e.key !== "Tab") return;
  const stops = [...e.currentTarget.querySelectorAll<HTMLElement>(TABBABLE)].filter((el) => el.tabIndex >= 0 && el.getClientRects().length > 0);
  if (stops.length === 0) return;
  const first = stops[0];
  const last = stops[stops.length - 1];
  const at = document.activeElement;
  if (e.shiftKey ? at === first : at === last) {
    e.preventDefault();
    (e.shiftKey ? last : first).focus();
  }
}
