"use client";

import { useEffect } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeftIcon, ShieldAlertIcon } from "lucide-react";

import { FullPageSkeleton } from "@/components/full-page-skeleton";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import { adminMe } from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";
import { cn } from "@/lib/utils";

const NAV_ITEMS = [
  { href: "/admin", label: "Overview" },
  { href: "/admin/organizations", label: "Organizations" },
  { href: "/admin/users", label: "Users" },
  { href: "/admin/plans", label: "Plans" },
  { href: "/admin/audit-logs", label: "Audit logs" },
  { href: "/admin/system", label: "System" },
];

function isActive(pathname: string, href: string): boolean {
  if (href === "/admin") return pathname === "/admin";
  return pathname === href || pathname.startsWith(`${href}/`);
}

/**
 * The route group boundary for the platform staff console — a sibling of
 * `(dashboard)`, not nested inside it, so admin screens get their own chrome
 * entirely rather than inheriting the tenant sidebar/nav. Every route under
 * here reads and writes across every organization (docs/11-admin-panel.md
 * §1), which is exactly why it needs to look nothing like the tenant app.
 *
 * The gate here is cosmetic. `RequirePlatformRole` on the backend is the
 * actual authority — every /admin/* call re-checks it server-side regardless
 * of what this layout decided — but a staff member (or a tenant user who
 * guesses the URL) landing on a wall of 401/403'd queries before bouncing
 * back to /overview is still a bad console, so GET /admin/me is called here
 * specifically to decide that up front (execution plan Task 5.1).
 */
export default function AdminLayout({ children }: { children: React.ReactNode }) {
  const { status } = useSession();
  const router = useRouter();
  const pathname = usePathname();

  useEffect(() => {
    if (status === "anon") router.replace("/login");
  }, [status, router]);

  // GET /admin/me is the console guard's own call — deliberately not
  // GET /auth/me, which the tenant nav uses for a different purpose (see
  // the doc comment on MeResponse.platformRole in lib/api/endpoints.ts).
  const {
    data: adminProfile,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["admin", "me"],
    queryFn: adminMe,
    enabled: status === "authed",
    // A 401/403 here is deterministic for this account right now — retrying
    // would just delay the redirect below.
    retry: false,
  });

  useEffect(() => {
    if (isError && error instanceof ApiError && (error.status === 401 || error.status === 403)) {
      router.replace("/overview");
    }
  }, [isError, error, router]);

  if (status === "loading" || status === "anon") return <FullPageSkeleton />;
  if (isError) return null; // redirect in flight
  if (isLoading || !adminProfile) return <FullPageSkeleton />;

  return (
    <div className="flex min-h-screen flex-1 flex-col">
      {/* The one line every screen under /admin shares, full width, above
          everything else — the worst failure mode of this console is a
          staff member forgetting they've left the tenant boundary. */}
      <div
        className="flex flex-wrap items-center justify-between gap-3 border-b border-amber-600/40 bg-amber-400/20
          px-4 py-2 text-xs font-medium tracking-wide text-amber-950 uppercase sm:px-6 dark:bg-amber-500/15
          dark:text-amber-200"
      >
        <span className="flex items-center gap-2">
          <ShieldAlertIcon className="size-3.5 shrink-0" aria-hidden />
          Platform admin — you are outside the tenant boundary
        </span>
        <Button
          size="sm"
          variant="ghost"
          render={<Link href="/overview" />}
          className="h-6 gap-1 px-2 text-[0.6875rem] font-medium tracking-normal text-amber-950 normal-case
            hover:bg-amber-500/20 dark:text-amber-200"
        >
          <ArrowLeftIcon className="size-3" /> Back to app
        </Button>
      </div>

      <div className="flex min-w-0 flex-1">
        <aside className="hidden w-52 shrink-0 flex-col gap-1 border-r bg-muted/20 px-3 py-5 md:flex">
          <div className="font-display px-2.5 pb-4 text-[0.9375rem] leading-none text-amber-800 dark:text-amber-300">
            admin console
          </div>
          <nav className="flex flex-col gap-1">
            {NAV_ITEMS.map((item) => {
              const active = isActive(pathname, item.href);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "rounded-md px-2.5 py-1.5 text-sm transition-colors",
                    "focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none",
                    active
                      ? "bg-amber-500/15 font-medium text-amber-900 dark:text-amber-200"
                      : "text-muted-foreground hover:bg-amber-500/10 hover:text-amber-900 dark:hover:text-amber-200",
                  )}
                >
                  {item.label}
                </Link>
              );
            })}
          </nav>
          <div className="mt-auto space-y-0.5 px-2.5 pt-4 text-[0.6875rem] text-muted-foreground">
            <div className="truncate font-mono">{adminProfile.email}</div>
            <div className="uppercase">{adminProfile.platformRole}</div>
          </div>
        </aside>

        <nav className="flex gap-1 overflow-x-auto border-b px-3 py-2 md:hidden">
          {NAV_ITEMS.map((item) => {
            const active = isActive(pathname, item.href);
            return (
              <Link
                key={item.href}
                href={item.href}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "shrink-0 rounded-md px-2.5 py-1.5 text-sm whitespace-nowrap transition-colors",
                  active
                    ? "bg-amber-500/15 font-medium text-amber-900 dark:text-amber-200"
                    : "text-muted-foreground hover:bg-amber-500/10",
                )}
              >
                {item.label}
              </Link>
            );
          })}
        </nav>

        <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-7 sm:px-6 sm:py-9">{children}</main>
      </div>
    </div>
  );
}
