"use client";

import { useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "next-themes";

import { Toaster } from "@/components/ui/sonner";
import { SessionProvider } from "@/lib/auth/use-session";

export function Providers({ children }: { children: React.ReactNode }) {
  const [queryClient] = useState(() => new QueryClient());

  return (
    // attribute="class" matches the `dark` variant in globals.css
    // (`@custom-variant dark (&:is(.dark *))`), which keys off a class on
    // <html>. disableTransitionOnChange suppresses the color transitions the
    // nav and table rows carry, so switching themes snaps instead of smearing.
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <QueryClientProvider client={queryClient}>
        <SessionProvider>
          {children}
          <Toaster />
        </SessionProvider>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
