"use client";

import { Suspense, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import { verifyEmail } from "@/lib/api/endpoints";

type Status = "verifying" | "success" | "error";

function VerifyEmailContent() {
  const token = useSearchParams().get("token");
  const queryClient = useQueryClient();

  const [status, setStatus] = useState<Status>(token ? "verifying" : "error");
  const [message, setMessage] = useState(
    token ? "" : "This verification link is missing its token.",
  );

  // React StrictMode double-invokes effects in dev, and the verification
  // token is single-use server-side (GETDEL-consumed) — a second POST would
  // consume-then-400 on an already-spent token, making a working flow look
  // broken. The ref makes the call fire at most once per mount no matter how
  // many times the effect body runs.
  const called = useRef(false);

  useEffect(() => {
    if (!token || called.current) return;
    called.current = true;

    verifyEmail({ token })
      .then(() => {
        setStatus("success");
        // Clears the verification banner immediately instead of waiting for
        // its own query to next refetch.
        void queryClient.invalidateQueries({ queryKey: ["me"] });
      })
      .catch((err: unknown) => {
        setStatus("error");
        setMessage(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
      });
  }, [token, queryClient]);

  if (status === "verifying") {
    return <p className="text-sm text-muted-foreground">Verifying your email…</p>;
  }

  if (status === "success") {
    return (
      <div className="flex flex-col gap-4">
        <p className="text-sm text-foreground">Your email is verified.</p>
        <Button className="w-full" render={<Link href="/organizations" />}>
          Continue
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-destructive">{message}</p>
      <p className="text-sm text-muted-foreground">
        Already verified, or the link expired?{" "}
        <Link href="/login" className="text-signal underline-offset-4 hover:underline">
          Sign in to request a new link
        </Link>
        .
      </p>
    </div>
  );
}

export default function VerifyEmailPage() {
  return (
    <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
      <h1 className="text-base font-semibold">Verify your email</h1>
      {/* useSearchParams needs a Suspense boundary above it for the
          production build (missing-suspense-with-csr-bailout); dev doesn't
          suspend so this is easy to miss without it. */}
      <Suspense fallback={<p className="text-sm text-muted-foreground">Loading…</p>}>
        <VerifyEmailContent />
      </Suspense>
    </div>
  );
}
