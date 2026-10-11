import path from "node:path";
import type { NextConfig } from "next";
import { withLeadline } from "@leadline/next";

const nextConfig: NextConfig = {
  // Only for this repository: the example links to @leadline/next by path,
  // which puts the package outside this folder, and Turbopack reads nothing
  // outside its root. An app that installs the package needs neither line.
  turbopack: { root: path.join(__dirname, "../..") },
  agentRules: false,
};

// All of the server side of Leadline is this call.
export default withLeadline(nextConfig);
