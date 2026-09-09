import type { MetadataRoute } from "next";

// Served at /manifest.webmanifest; Next emits the <link rel="manifest"> tag
// itself. `icon-512.png` has no other consumer -- Android home-screen and the
// PWA install prompt read it from here.
export default function manifest(): MetadataRoute.Manifest {
  return {
    name: "Sapanjai",
    short_name: "Sapanjai",
    description:
      "Managed MCP gateway — connect an agent to your systems, scoped and audited.",
    start_url: "/",
    display: "standalone",
    // The ink ground the icon sits on, so the splash screen matches the tab.
    background_color: "#111621",
    theme_color: "#111621",
    icons: [
      {
        src: "/icon-512.png",
        sizes: "512x512",
        type: "image/png",
        // "any" only: the artwork is full-bleed, not safe-zone padded, so
        // letting Android maskable-crop it would clip the posts.
        purpose: "any",
      },
    ],
  };
}
