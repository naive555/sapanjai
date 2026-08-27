"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { zodResolver } from "@hookform/resolvers/zod";
import { Controller, useForm } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api/client";
import { login } from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

const loginSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(1, "Password is required"),
});

type LoginFormValues = z.infer<typeof loginSchema>;

export default function LoginPage() {
  const router = useRouter();
  const { applyTokens } = useSession();
  const {
    control,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<LoginFormValues>({
    resolver: zodResolver(loginSchema),
    defaultValues: { email: "", password: "" },
  });

  const onSubmit = async (values: LoginFormValues) => {
    try {
      const tokens = await login(values);
      applyTokens(tokens);
      // The overview is the post-login landing (Phase 5). It handles an
      // unselected org itself — a returning user on a fresh browser has no
      // stored selection — so this doesn't need to route around that case.
      router.replace("/overview");
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
    }
  };

  return (
    <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
      <h1 className="text-base font-semibold">Log in</h1>

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

          <Field data-invalid={!!errors.password}>
            <div className="flex items-center justify-between">
              <FieldLabel htmlFor="password">Password</FieldLabel>
              <Link href="/forgot-password" className="text-signal text-sm underline-offset-4 hover:underline">
                Forgot password?
              </Link>
            </div>
            <Controller
              control={control}
              name="password"
              render={({ field }) => (
                <Input id="password" type="password" autoComplete="current-password" {...field} />
              )}
            />
            <FieldError errors={[errors.password]} />
          </Field>

          <Button type="submit" disabled={isSubmitting} className="w-full">
            {isSubmitting ? "Logging in…" : "Log in"}
          </Button>
        </FieldGroup>
      </form>

      <p className="text-sm text-muted-foreground">
        No account?{" "}
        <Link href="/register" className="text-signal underline-offset-4 hover:underline">
          Create one
        </Link>
      </p>
    </div>
  );
}
