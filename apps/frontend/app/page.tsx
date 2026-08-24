"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

import { useSession } from "@/lib/auth/use-session";

export default function Home() {
  const { status } = useSession();
  const router = useRouter();

  useEffect(() => {
    if (status === "authed") {
      router.replace("/organizations");
    } else if (status === "anon") {
      router.replace("/login");
    }
  }, [status, router]);

  // Every visitor sees this for the instant it takes to resolve session
  // status and redirect, so it is the product's actual first frame — a quiet
  // wordmark and the thesis, not a loading skeleton with nothing to say.
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <span className="font-display text-base leading-none">
        Sapan<span className="text-signal">jai</span>
      </span>
      <p className="text-sm text-muted-foreground">Give an agent a safe door into your systems.</p>
    </div>
  );
}
