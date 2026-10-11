import type { LeadlineOptions } from "@leadline/node";

/** What `withLeadline(nextConfig, options)` takes. Everything is optional. */
export interface LeadlineNextOptions
  extends Pick<
    LeadlineOptions,
    | "url"
    | "serviceName"
    | "enabled"
    | "console"
    | "metrics"
    | "batchDelayMs"
    | "instrumentationConfig"
    | "instrumentations"
    | "debug"
  > {
  /**
   * Work out who the user is from the session cookie of Auth.js / NextAuth,
   * Clerk or Better Auth, where that needs no request to anything.
   * Default: on. `identify()` always works and always wins.
   */
  users?: boolean;
  /** Send the app's route list when the dev server starts and when it changes. Default: on. */
  routes?: boolean;
  /**
   * Keep these packages out of the server bundle in development, so their
   * queries are seen. Default: the database drivers Leadline instruments
   * that Next.js would otherwise bundle. `false` leaves the list alone.
   */
  externalPackages?: string[] | false;
}

/** Who made a request. Only what is given is sent. */
export interface LeadlineUser {
  id: string | number;
  email?: string | null;
  name?: string | null;
}

/** One entry of the route list sent to Leadline. */
export interface RouteEntry {
  /** The pattern as Next.js names it: `/blog/[slug]`. Empty for the proxy. */
  route: string;
  kind: "page" | "route" | "layout" | "api" | "metadata" | "proxy";
  router: "app" | "pages";
  /** The file, relative to the project. */
  file: string;
  /** True when the pattern has a `[param]`. */
  dynamic: boolean;
  /** A route handler's exported methods, where they could be read. */
  methods?: string[];
  /** A page's layouts, outermost first. */
  layouts?: string[];
}
