import type { Metadata } from "next";
import { IBM_Plex_Mono, IBM_Plex_Sans, Martian_Mono } from "next/font/google";
import "./globals.css";
import { Providers } from "./providers";

// Display face. Wide and engineered — carries the wordmark and page titles
// only. Every heading in this product names an identifier, so the display
// face is a mono too, just a dramatically different one from the data face.
const martianMono = Martian_Mono({
  variable: "--font-martian",
  subsets: ["latin"],
});

// Body face for prose, labels, and controls.
const plexSans = IBM_Plex_Sans({
  variable: "--font-plex-sans",
  subsets: ["latin"],
  weight: ["400", "500", "600"],
});

// Data face for slugs, permission strings, audit actions, timestamps, UUIDs.
const plexMono = IBM_Plex_Mono({
  variable: "--font-plex-mono",
  subsets: ["latin"],
  weight: ["400", "500", "600"],
});

export const metadata: Metadata = {
  title: "Sapanjai",
  description: "Multi-tenant B2B SaaS platform dashboard",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    // suppressHydrationWarning is required by next-themes: it writes the
    // theme class onto <html> in a pre-hydration script, so the server markup
    // and the hydrating client markup differ on this element by design.
    <html
      lang="en"
      suppressHydrationWarning
      className={`${martianMono.variable} ${plexSans.variable} ${plexMono.variable} h-full antialiased`}
    >
      <body className="flex min-h-full flex-col">
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
