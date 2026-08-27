"use client";

import { useState } from "react";
import Link from "next/link";
import { zodResolver } from "@hookform/resolvers/zod";
import { Controller, useForm } from "react-hook-form";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { forgotPassword } from "@/lib/api/endpoints";

const forgotPasswordSchema = z.object({
  email: z.email("Enter a valid email address"),
});

type ForgotPasswordFormValues = z.infer<typeof forgotPasswordSchema>;

export default function ForgotPasswordPage() {
  // Set on submit and never unset — there's no path back to the form,
  // matching the enumeration-safe contract's one-shot "we've sent it" reply.
  const [sent, setSent] = useState(false);
  const {
    control,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<ForgotPasswordFormValues>({
    resolver: zodResolver(forgotPasswordSchema),
    defaultValues: { email: "" },
  });

  const onSubmit = async (values: ForgotPasswordFormValues) => {
    // POST /auth/forgot-password is always 200-shaped so a caller can't
    // learn whether the address exists (docs/02-api-contract.md). The catch
    // here extends that guarantee through a network error or a backend 5xx:
    // the same panel renders either way, never an error toast that would
    // leak a signal the success path doesn't carry.
    try {
      await forgotPassword(values);
    } catch {
      // Deliberately swallowed — see above.
    } finally {
      setSent(true);
    }
  };

  if (sent) {
    return (
      <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
        <h1 className="text-base font-semibold">Check your email</h1>
        <p className="text-sm text-muted-foreground">
          If an account exists for that address, we&apos;ve sent a reset link.
        </p>
        <Link href="/login" className="text-signal text-sm underline-offset-4 hover:underline">
          Back to login
        </Link>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
      <h1 className="text-base font-semibold">Forgot password</h1>

      <form onSubmit={handleSubmit(onSubmit)} noValidate>
        <FieldGroup>
          <Field data-invalid={!!errors.email}>
            <FieldLabel htmlFor="email">Email</FieldLabel>
            <Controller
              control={control}
              name="email"
              render={({ field }) => (
                <Input
                  id="email"
                  type="email"
                  autoComplete="email"
                  className="font-mono"
                  placeholder="you@example.com"
                  {...field}
                />
              )}
            />
            <FieldError errors={[errors.email]} />
          </Field>

          <Button type="submit" disabled={isSubmitting} className="w-full">
            {isSubmitting ? "Sending…" : "Send reset link"}
          </Button>
        </FieldGroup>
      </form>

      <p className="text-sm text-muted-foreground">
        Remembered it?{" "}
        <Link href="/login" className="text-signal underline-offset-4 hover:underline">
          Log in
        </Link>
      </p>
    </div>
  );
}
