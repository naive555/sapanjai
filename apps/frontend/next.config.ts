import type { NextConfig } from "next";

// No rewrites() here for /api/* — that's handled by the runtime Route
// Handler at app/api/[...path]/route.ts instead (see its top comment for
// why: next.config rewrites are resolved at build time, not request time).
const nextConfig: NextConfig = {
  output: "standalone",
};

export default nextConfig;
