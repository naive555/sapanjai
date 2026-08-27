"use client";

import { Suspense, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { zodResolver } from "@hookform/resolvers/zod";
import { Controller, useForm } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api/client";
import { resetPassword } from "@/lib/api/endpoints";

const resetPasswordSchema = z
  .object({
    password: z.string().min(8, "Password must be at least 8 characters"),
    confirmPassword: z.string().min(1, "Confirm your password"),
  })
  .refine((values) => values.password === values.confirmPassword, {
    message: "Passwords don't match",
    path: ["confirmPassword"],
  });

type ResetPasswordFormValues = z.infer<typeof resetPasswordSchema>;

function ResetPasswordContent() {
  const router = useRouter();
  const token = useSearchParams().get("token");
  const [serverError, setServerError] = useState<string | null>(null);

  const {
    control,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<ResetPasswordFormValues>({
    resolver: zodResolver(resetPasswordSchema),
    defaultValues: { password: "", confirmPassword: "" },
  });

  // A reset link can never succeed without its token — render the same
  // "get a new one" affordance the failed-submit path uses below, rather
  // than letting the form submit into a guaranteed INVALID_RESET_TOKEN.
  if (!token) {
    return (
      <div className="flex flex-col gap-4">
        <p className="text-sm text-destructive">This reset link is missing its token.</p>
        <Link href="/forgot-password" className="text-signal text-sm underline-offset-4 hover:underline">
          Request a new link
        </Link>
      </div>
    );
  }

  const onSubmit = async (values: ResetPasswordFormValues) => {
    setServerError(null);
    try {
      await resetPassword({ token, password: values.password });
      toast.success("Password reset. Log in with your new password.");
      router.replace("/login");
    } catch (err) {
      setServerError(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
    }
  };

  return (
    <form onSubmit={handleSubmit(onSubmit)} noValidate>
      <FieldGroup>
        <Field data-invalid={!!errors.password}>
          <FieldLabel htmlFor="password">New password</FieldLabel>
          <Controller
            control={control}
            name="password"
            render={({ field }) => (
              <Input id="password" type="password" autoComplete="new-password" {...field} />
            )}
          />
          <FieldError errors={[errors.password]} />
        </Field>

        <Field data-invalid={!!errors.confirmPassword}>
          <FieldLabel htmlFor="confirmPassword">Confirm password</FieldLabel>
          <Controller
            control={control}
            name="confirmPassword"
            render={({ field }) => (
              <Input id="confirmPassword" type="password" autoComplete="new-password" {...field} />
            )}
          />
          <FieldError errors={[errors.confirmPassword]} />
        </Field>

        {serverError && (
          <div className="flex flex-col gap-1">
            <p className="text-sm text-destructive">{serverError}</p>
            <Link href="/forgot-password" className="text-signal text-sm underline-offset-4 hover:underline">
              Request a new link
            </Link>
          </div>
        )}

        <Button type="submit" disabled={isSubmitting} className="w-full">
          {isSubmitting ? "Resetting…" : "Reset password"}
        </Button>
      </FieldGroup>
    </form>
  );
}

export default function ResetPasswordPage() {
  return (
    <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
      <h1 className="text-base font-semibold">Reset password</h1>
      {/* useSearchParams needs a Suspense boundary above it for the
          production build (missing-suspense-with-csr-bailout). */}
      <Suspense fallback={<p className="text-sm text-muted-foreground">Loading…</p>}>
        <ResetPasswordContent />
      </Suspense>
    </div>
  );
}
