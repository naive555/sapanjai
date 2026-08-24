import type { NextConfig } from "next";

// No rewrites() here for /api/* — that's handled by the runtime Route
// Handler at app/api/[...path]/route.ts instead (see its top comment for
// why: next.config rewrites are resolved at build time, not request time).
// redirects() below is a different animal: it's a static path-to-path
// mapping with no dependency on a runtime value like BACKEND_URL, so
// resolving it at build time is fine — it doesn't hit the problem the
// comment above is about.
const nextConfig: NextConfig = {
  output: "standalone",
  async redirects() {
    return [
      {
        source: "/audit",
        destination: "/activity",
        permanent: true,
      },
    ];
  },
};

export default nextConfig;
