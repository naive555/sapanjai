"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { useSession } from "@/lib/auth/use-session";

function formatCountdown(msRemaining: number): string {
  const totalSeconds = Math.max(0, Math.floor(msRemaining / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, "0")}`;
}

/**
 * Renders while a staff member is impersonating a tenant user — see
 * docs/11-admin-panel.md §5's threat model and execution plan Task 5.2. The
 * single worst failure mode this whole feature can produce is a staff
 * member forgetting which identity they're driving, so this is full-width,
 * sits above everything else in the dashboard shell, and is amber rather
 * than any color the tenant theme itself uses — there is no scroll position
 * or nav state from which it isn't visible, and nothing about it should
 * ever look like an ordinary part of the app.
 *
 * The countdown is cosmetic — it re-derives from `impersonationTarget.
 * expiresAt`, which was computed once from the impersonate response's
 * `expiresIn` and never adjusted afterward. The backend is the actual clock
 * (Task 5.2's dangerous-401 path in lib/api/client.ts is what fires when
 * this number hits zero for real); this is just so a support engineer isn't
 * caught by surprise.
 */
export function ImpersonationBanner() {
  const { impersonating, impersonationTarget, exitImpersonation } = useSession();
  const router = useRouter();
  const [now, setNow] = useState(() => Date.now());
  const [exiting, setExiting] = useState(false);

  useEffect(() => {
    if (!impersonating) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [impersonating]);

  if (!impersonating || !impersonationTarget) return null;

  const handleExit = async () => {
    setExiting(true);
    try {
      await exitImpersonation();
      toast.success("Exited impersonation — you're back in your own account.");
      router.push(`/admin/users/${impersonationTarget.userId}`);
    } catch {
      // exitImpersonation's own restoreAdminSession swallows a failed
      // refresh internally (falling back to "anon"), so this branch is
      // effectively unreachable in practice — kept for the same reason
      // every other mutation handler here has an onError/catch: silent
      // failure would leave the banner stuck rendering with no way out.
      toast.error("Failed to exit impersonation cleanly — reload if the banner doesn't clear.");
    } finally {
      setExiting(false);
    }
  };

  const remaining = formatCountdown(impersonationTarget.expiresAt - now);

  return (
    <div
      className="flex flex-wrap items-center justify-between gap-3 border-b border-amber-600/40 bg-amber-400/20 px-4
        py-2 text-sm text-amber-950 sm:px-6 dark:bg-amber-500/15 dark:text-amber-200"
    >
      <span>
        Viewing as <span className="font-mono font-medium">{impersonationTarget.email}</span> · read-only · expires
        in <span className="font-mono tabular-nums">{remaining}</span>
      </span>
      <Button
        size="sm"
        variant="outline"
        disabled={exiting}
        onClick={() => void handleExit()}
        className="border-amber-700/40 bg-transparent text-amber-950 hover:bg-amber-500/20 dark:text-amber-100"
      >
        {exiting ? "Exiting…" : "Exit"}
      </Button>
    </div>
  );
}
