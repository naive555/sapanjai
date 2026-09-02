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
import { register as registerRequest } from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

const registerSchema = z.object({
  email: z.email("Enter a valid email address"),
  password: z.string().min(8, "Password must be at least 8 characters"),
  displayName: z.string().optional(),
});

type RegisterFormValues = z.infer<typeof registerSchema>;

export default function RegisterPage() {
  const router = useRouter();
  const { applyTokens } = useSession();
  const {
    control,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<RegisterFormValues>({
    resolver: zodResolver(registerSchema),
    defaultValues: { email: "", password: "", displayName: "" },
  });

  const onSubmit = async (values: RegisterFormValues) => {
    try {
      const tokens = await registerRequest({
        email: values.email,
        password: values.password,
        displayName: values.displayName?.trim() || undefined,
      });
      applyTokens(tokens);
      // Same landing as login, for a sharper reason here: someone who just
      // registered has no organization at all yet, and this is the page that
      // creates one.
      router.replace("/organizations");
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
    }
  };

  return (
    <div className="flex flex-col gap-5 rounded-lg border bg-card p-6">
      <h1 className="text-base font-semibold">Create an account</h1>

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
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <Controller
              control={control}
              name="password"
              render={({ field }) => (
                <Input id="password" type="password" autoComplete="new-password" {...field} />
              )}
            />
            <FieldError errors={[errors.password]} />
          </Field>

          <Field data-invalid={!!errors.displayName}>
            <FieldLabel htmlFor="displayName">Display name (optional)</FieldLabel>
            <Controller
              control={control}
              name="displayName"
              render={({ field }) => <Input id="displayName" autoComplete="name" {...field} />}
            />
            <FieldError errors={[errors.displayName]} />
          </Field>

          <Button type="submit" disabled={isSubmitting} className="w-full">
            {isSubmitting ? "Creating account…" : "Create account"}
          </Button>
        </FieldGroup>
      </form>

      <p className="text-sm text-muted-foreground">
        Already have an account?{" "}
        <Link href="/login" className="text-signal underline-offset-4 hover:underline">
          Log in
        </Link>
      </p>
    </div>
  );
}
