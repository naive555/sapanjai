"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon, CheckIcon, ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api/client";
import {
  confirmTOTP,
  enrollTOTP,
  verifyTOTP,
  type TOTPConfirmResponse,
  type TOTPEnrollResponse,
} from "@/lib/api/endpoints";
import { copyToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";

type Mode = "verify" | "enroll";

/** Shared shell for the boxed, monospace "copy this" blocks below. */
function CopyBox({ value, className }: { value: string; className?: string }) {
  return (
    <div className={cn("rounded-md border bg-muted/40 p-3", className)}>
      <code className="block w-full overflow-x-auto font-mono text-sm break-all select-all">{value}</code>
    </div>
  );
}

// Pulled client-side from the otpauth:// URI's own `secret` query param —
// there is no separate field for it, and CLAUDE.md's "no new dependencies"
// constraint rules out a QR-code library, so this plus the raw URI above it
// are the only two things an authenticator app needs. Returns null rather
// than throwing on a URI shaped unexpectedly; the caller falls back to just
// showing the URI.
function parseOtpauthSecret(otpauthUri: string): string | null {
  try {
    return new URL(otpauthUri).searchParams.get("secret");
  } catch {
    return null;
  }
}

/**
 * The TOTP enrollment + step-up console — the missing UI for
 * `POST /admin/2fa/{enroll,confirm,verify}` (apps/backend/internal/module/admin/{handler,totp}.go).
 * Reachable at /admin/2fa, deliberately exempted from the (admin)/admin
 * layout's own GET /admin/me gate (see that file's STEP_UP_PATH) — this
 * page exists specifically for the moment that call is 403ing with
 * TWO_FACTOR_REQUIRED_MESSAGE, so it can never depend on it succeeding.
 * Closed to non-staff anyway: the layout's redirect effect keeps running
 * regardless of path and bounces an ordinary 401/403 (no platform role at
 * all) to /overview, and every mutation below is re-checked server-side by
 * RequirePlatformRoleNo2FA regardless of what this page renders.
 *
 * There is no "am I enrolled?" endpoint (docs/02-api-contract.md has none,
 * and CLAUDE.md's task here is explicit: don't invent one). So the default
 * view is the verify form — the common case for a staff member who has
 * already enrolled and just needs a fresh 12h step-up — and enrollment is
 * one click away for the first-time case. If verify comes back
 * TOTP_NOT_ENROLLED (400 — the one 400 this endpoint defines), that IS the
 * "not enrolled yet" signal the API actually gives us, so this switches to
 * the enroll flow automatically rather than making the staff member guess.
 */
export default function AdminTwoFactorPage() {
  const router = useRouter();
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<Mode>("verify");

  // ---- verify (also the tail end of a fresh enrollment, below) ----
  const [verifyCode, setVerifyCode] = useState("");
  const verifyMutation = useMutation({
    mutationFn: () => verifyTOTP(verifyCode.trim()),
    onSuccess: async () => {
      toast.success("Two-factor verified — good for the next 12 hours.");
      // The (admin)/admin layout's GET /admin/me query is still mounted the
      // whole time this page is (same layout instance, just a different
      // child route) and is what every other /admin screen's gate depends
      // on. Without this, it would still be holding its stale
      // TWO_FACTOR_REQUIRED_MESSAGE error from before verify ran, and the
      // push below would just bounce straight back here. Awaited so the
      // cache is already correct by the time the navigation lands.
      await queryClient.invalidateQueries({ queryKey: ["admin", "me"] });
      router.push("/admin");
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 400) {
        // TOTP_NOT_ENROLLED — the only 400 this endpoint returns. Nothing
        // to verify against yet, so send them to set one up instead of
        // showing a dead-end error.
        setMode("enroll");
        toast.error("No authenticator enrolled yet — set one up below.");
        return;
      }
      toast.error(err instanceof ApiError ? err.message : "Verification failed.");
    },
  });

  // ---- enroll: stage 1, generate a secret ----
  const [enrollment, setEnrollment] = useState<TOTPEnrollResponse | null>(null);
  const enrollMutation = useMutation({
    mutationFn: enrollTOTP,
    onSuccess: (resp) => {
      setEnrollment(resp);
      setConfirmCode("");
      setConfirmResult(null);
      setCodesAcked(false);
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to start enrollment."),
  });
  const secret = useMemo(() => (enrollment ? parseOtpauthSecret(enrollment.otpauthUri) : null), [enrollment]);

  // ---- enroll: stage 2, confirm the first code ----
  const [confirmCode, setConfirmCode] = useState("");
  const [confirmResult, setConfirmResult] = useState<TOTPConfirmResponse | null>(null);
  const confirmMutation = useMutation({
    mutationFn: () => confirmTOTP(confirmCode.trim()),
    onSuccess: (resp) => {
      setConfirmResult(resp);
      toast.success("Authenticator confirmed.");
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Confirmation failed."),
  });

  // ---- enroll: stage 3, the ten recovery codes, shown exactly once ----
  const [codesAcked, setCodesAcked] = useState(false);

  function switchToEnroll() {
    setMode("enroll");
  }

  function switchToVerify() {
    setMode("verify");
    setVerifyCode("");
  }

  function finishEnrollment() {
    // Recovery codes are never persisted client-side (CLAUDE.md: never log
    // or render a recovery code anywhere but this page) — dropping enroll
    // state here is what actually clears them from memory, not just from
    // view.
    setEnrollment(null);
    setConfirmResult(null);
    setCodesAcked(false);
    switchToVerify();
    toast.success("Authenticator enrolled — enter a fresh code below to finish signing in.");
  }

  async function handleCopyRecoveryCodes(codes: string[]) {
    if (await copyToClipboard(codes.join("\n"))) {
      toast.success("Recovery codes copied.");
    } else {
      toast.error("Couldn't copy automatically — select and copy them by hand.");
    }
  }

  return (
    <div className="mx-auto flex min-h-screen w-full max-w-xl flex-col gap-6 px-4 py-10 sm:px-6">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <ShieldCheckIcon className="size-5 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden />
          <h1 className="font-display text-xl leading-none font-medium">two-factor step-up</h1>
        </div>
        <Button variant="ghost" size="sm" render={<Link href="/overview" />}>
          <ArrowLeftIcon className="size-3.5" /> Back to app
        </Button>
      </div>
      <p className="text-sm text-muted-foreground">
        Platform staff accounts require a TOTP code every 12 hours before the admin console will load —
        that&apos;s what sent you here. Verify with your authenticator (or a recovery code) below, or set one up
        for the first time.
      </p>

      <div className="flex gap-1 rounded-lg border bg-muted/20 p-1">
        <button
          type="button"
          onClick={switchToVerify}
          aria-pressed={mode === "verify"}
          className={cn(
            "flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
            mode === "verify" ? "bg-card shadow-sm" : "text-muted-foreground hover:text-foreground",
          )}
        >
          I have a code
        </button>
        <button
          type="button"
          onClick={switchToEnroll}
          aria-pressed={mode === "enroll"}
          className={cn(
            "flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
            mode === "enroll" ? "bg-card shadow-sm" : "text-muted-foreground hover:text-foreground",
          )}
        >
          Set up / replace authenticator
        </button>
      </div>

      {mode === "verify" && (
        <section className="space-y-4 rounded-lg border bg-card p-4">
          <Field>
            <FieldLabel htmlFor="verify-code">Authenticator code or recovery code</FieldLabel>
            <Input
              id="verify-code"
              autoFocus
              autoComplete="one-time-code"
              value={verifyCode}
              onChange={(e) => setVerifyCode(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && verifyCode.trim() && !verifyMutation.isPending) verifyMutation.mutate();
              }}
              placeholder="123456"
            />
            <FieldDescription>
              A live 6-digit code from your authenticator app, or one of your ten recovery codes. A recovery code
              is consumed the moment it&apos;s used.
            </FieldDescription>
          </Field>
          <Button
            disabled={!verifyCode.trim() || verifyMutation.isPending}
            onClick={() => verifyMutation.mutate()}
            className="w-full"
          >
            {verifyMutation.isPending ? "Verifying…" : "Verify"}
          </Button>
        </section>
      )}

      {mode === "enroll" && (
        <section className="space-y-4 rounded-lg border border-destructive/40 p-4">
          <div className="space-y-1">
            <h2 className="font-heading text-base font-medium text-destructive">
              {enrollment ? "New enrollment in progress" : "Set up a new authenticator"}
            </h2>
            <p className="text-sm text-muted-foreground">
              If you already have an authenticator enrolled, generating a new secret invalidates it and every
              existing recovery code immediately — only do this if you&apos;ve lost access to the old one or are
              setting up for the first time.
            </p>
          </div>

          {!enrollment && (
            <Button
              variant="destructive"
              disabled={enrollMutation.isPending}
              onClick={() => enrollMutation.mutate()}
            >
              {enrollMutation.isPending ? "Generating…" : "Generate new secret"}
            </Button>
          )}

          {enrollment && !confirmResult && (
            <div className="space-y-4">
              <div className="space-y-2">
                <FieldLabel>1. Add this to your authenticator app</FieldLabel>
                <p className="text-sm text-muted-foreground">
                  Paste the URI below into an authenticator that accepts an{" "}
                  <code className="font-mono text-xs">otpauth://</code> link (1Password, Bitwarden, and most
                  desktop TOTP tools do). If your app only takes a manual secret, use the key underneath instead —
                  standard settings apply: SHA-1, 6 digits, 30-second period.
                </p>
                <CopyBox value={enrollment.otpauthUri} />
                {secret && (
                  <>
                    <FieldLabel className="text-xs text-muted-foreground">Manual entry key</FieldLabel>
                    <CopyBox value={secret} />
                  </>
                )}
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  disabled={enrollMutation.isPending}
                  onClick={() => enrollMutation.mutate()}
                >
                  Wrong app or mis-scanned? Generate a different secret
                </Button>
              </div>

              <Field>
                <FieldLabel htmlFor="confirm-code">2. Enter the current code it shows</FieldLabel>
                <Input
                  id="confirm-code"
                  autoComplete="one-time-code"
                  value={confirmCode}
                  onChange={(e) => setConfirmCode(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && confirmCode.trim() && !confirmMutation.isPending) {
                      confirmMutation.mutate();
                    }
                  }}
                  placeholder="123456"
                  maxLength={6}
                />
              </Field>
              <Button
                variant="destructive"
                disabled={confirmCode.trim().length !== 6 || confirmMutation.isPending}
                onClick={() => confirmMutation.mutate()}
                className="w-full"
              >
                {confirmMutation.isPending ? "Confirming…" : "Confirm"}
              </Button>
            </div>
          )}

          {confirmResult && (
            <div className="space-y-3">
              <div className="space-y-1">
                <FieldLabel>3. Save your recovery codes</FieldLabel>
                <p className="text-sm font-medium text-destructive">
                  These are shown exactly once. Each one signs you in a single time if you lose your
                  authenticator — store them somewhere safe now.
                </p>
              </div>
              <div className="grid grid-cols-2 gap-x-4 gap-y-1 rounded-md border bg-muted/40 p-3 font-mono text-sm">
                {confirmResult.recoveryCodes.map((code) => (
                  <span key={code} className="select-all">
                    {code}
                  </span>
                ))}
              </div>
              <Button
                type="button"
                variant="outline"
                className="w-full"
                onClick={() => void handleCopyRecoveryCodes(confirmResult.recoveryCodes)}
              >
                Copy all
              </Button>

              <Field orientation="horizontal" className="items-start gap-2 pt-2">
                <Checkbox
                  id="codes-acked"
                  checked={codesAcked}
                  onCheckedChange={(checked) => setCodesAcked(checked === true)}
                  className="mt-0.5"
                />
                <FieldLabel htmlFor="codes-acked" className="text-sm font-normal">
                  I&apos;ve saved these recovery codes somewhere safe. I understand they will not be shown
                  again.
                </FieldLabel>
              </Field>
              <Button disabled={!codesAcked} onClick={finishEnrollment} className="w-full">
                <CheckIcon className="size-4" /> Continue to verify
              </Button>
            </div>
          )}
        </section>
      )}
    </div>
  );
}
