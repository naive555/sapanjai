"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

import { FullPageSkeleton } from "@/components/full-page-skeleton";
import { ThemeToggle } from "@/components/theme-toggle";
import { useSession } from "@/lib/auth/use-session";

export default function AuthLayout({ children }: { children: React.ReactNode }) {
  const { status } = useSession();
  const router = useRouter();

  useEffect(() => {
    if (status === "authed") {
      router.replace("/overview");
    }
  }, [status, router]);

  if (status === "loading") return <FullPageSkeleton />;
  if (status === "authed") return null; // redirect in flight

  return (
    <div className="flex flex-1 items-center justify-center p-6">
      <div className="flex w-full max-w-sm flex-col gap-7">
        <div className="flex items-start justify-between gap-4">
          <div className="flex flex-col gap-2">
            <span className="font-display text-base leading-none">
              Sapan<span className="text-signal">jai</span>
            </span>
            <p className="text-sm text-muted-foreground">
              Give an agent a safe door into your systems.
            </p>
          </div>
          <ThemeToggle />
        </div>
        {children}
      </div>
    </div>
  );
}
