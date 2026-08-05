"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

import { FullPageSkeleton } from "@/components/full-page-skeleton";
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

  return <FullPageSkeleton />;
}
