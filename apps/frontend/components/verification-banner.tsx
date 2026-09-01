"use client";

import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { XIcon } from "lucide-react";
import { toast } from "sonner";

import { Callout } from "@/components/callout";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import { me, resendVerification } from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

// Per-tab, deliberately not localStorage: the point of this banner is a nag,
// so dismissing it in one tab shouldn't silence it everywhere, and it comes
// back the next time the user opens the app in a fresh tab.
const DISMISS_KEY = "verification-banner-dismissed";

export function VerificationBanner() {
  const { impersonating } = useSession();

  // Read lazily (not in an effect — this component only ever mounts once
  // DashboardLayout has already committed status "authed" on the client, so
  // unlike use-session.tsx's token read there's no server/client markup to
  // mismatch here).
  const [dismissed, setDismissed] = useState(() => {
    try {
      return sessionStorage.getItem(DISMISS_KEY) === "1";
    } catch {
      // Storage unavailable (private browsing, disabled site data, ...) —
      // just don't persist; the banner still renders this session.
      return false;
    }
  });

  // Deliberately its own query, not folded into use-session.tsx — that file
  // is on the boot path with a hydration contract of its own. `isVerified`
  // has no bearing on whether the caller is authed at all.
  const { data: user, isLoading } = useQuery({ queryKey: ["me"], queryFn: me });

  const resendMutation = useMutation({
    mutationFn: resendVerification,
    onSuccess: () => toast.success("Verification email sent."),
    onError: (err: unknown) => {
      // Covers both ALREADY_VERIFIED and the 429 VERIFICATION_RESEND_TOO_SOON
      // cooldown message — both are already human-readable server text.
      toast.error(err instanceof ApiError ? err.message : "Failed to resend verification email.");
    },
  });

  // While impersonating, `user` above resolves to the impersonated TARGET
  // (see MeResponse.platformRole's doc comment in lib/api/endpoints.ts) —
  // its "Resend" button is a POST, which a read-only impersonation token
  // gets 403'd on by the guard, and even if it weren't, nagging a staff
  // member to verify a stranger's inbox makes no sense. Hide outright
  // rather than let it render a control that can only ever fail.
  if (isLoading || !user || user.isVerified || dismissed || impersonating) return null;

  const handleDismiss = () => {
    setDismissed(true);
    try {
      sessionStorage.setItem(DISMISS_KEY, "1");
    } catch {
      // Same as above — dismissal just won't survive a reload this session.
    }
  };

  return (
    <Callout
      variant="note"
      className="flex items-center justify-between gap-4 rounded-none border-x-0 border-t-0"
    >
      {/* Nothing is actually gated on isVerified server-side, so the copy
          asks rather than claiming a locked feature the user could go find. */}
      <span>Verify your email address — check your inbox for the link.</span>
      <div className="flex shrink-0 items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          disabled={resendMutation.isPending}
          onClick={() => resendMutation.mutate()}
        >
          {resendMutation.isPending ? "Sending…" : "Resend"}
        </Button>
        <Button size="icon-sm" variant="ghost" aria-label="Dismiss" onClick={handleDismiss}>
          <XIcon />
        </Button>
      </div>
    </Callout>
  );
}
