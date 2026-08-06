"use client";

import { useEffect } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { toast } from "sonner";

import { FullPageSkeleton } from "@/components/full-page-skeleton";
import { ScopeChain } from "@/components/scope-chain";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { useSession } from "@/lib/auth/use-session";
import { useActiveOrgId } from "@/lib/org/active-org";
import { cn } from "@/lib/utils";

// Grouped by what each route actually requires of a request, mirroring the
// backend's own middleware split: "account" routes need only a session,
// "tenant" routes additionally need the x-organization-id header. The
// grouping is the reason the second group greys out with no org selected.
const NAV_GROUPS: { label: string; requiresOrg: boolean; items: { href: string; label: string }[] }[] = [
  {
    label: "account",
    requiresOrg: false,
    items: [{ href: "/organizations", label: "Organizations" }],
  },
  {
    label: "tenant",
    requiresOrg: true,
    items: [
      { href: "/members", label: "Members" },
      { href: "/roles", label: "Roles" },
      { href: "/audit", label: "Audit log" },
      { href: "/subscription", label: "Subscription" },
    ],
  },
];

function NavLinks({ className }: { className?: string }) {
  const pathname = usePathname();
  const hasActiveOrg = useActiveOrgId() !== null;

  return (
    <>
      {NAV_GROUPS.map((group) => (
        <div key={group.label} className={className}>
          <div className="label-eyebrow mb-2 hidden px-2.5 md:block">{group.label}</div>
          <div className="flex gap-1 md:flex-col">
            {group.items.map((item) => {
              const disabled = group.requiresOrg && !hasActiveOrg;
              const active = pathname === item.href;

              if (disabled) {
                return (
                  <span
                    key={item.href}
                    title="Select an organization to reach this"
                    className="flex cursor-not-allowed items-center gap-2 rounded-md px-2.5 py-1.5 text-sm
                      whitespace-nowrap text-muted-foreground/45"
                  >
                    <span aria-hidden className="h-px w-2 [border-top:1px_dashed_currentColor]" />
                    {item.label}
                  </span>
                );
              }

              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "flex items-center gap-2 rounded-md px-2.5 py-1.5 text-sm whitespace-nowrap transition-colors",
                    "focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none",
                    active
                      ? "bg-accent font-medium text-foreground"
                      : "text-muted-foreground hover:bg-accent/60 hover:text-foreground",
                  )}
                >
                  <span
                    aria-hidden
                    className={cn("h-3.5 w-px shrink-0", active ? "bg-signal" : "bg-transparent")}
                  />
                  {item.label}
                </Link>
              );
            })}
          </div>
        </div>
      ))}
    </>
  );
}

export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  const { status, user, logoutSession } = useSession();
  const router = useRouter();

  useEffect(() => {
    if (status === "anon") {
      router.replace("/login");
    }
  }, [status, router]);

  if (status === "loading") return <FullPageSkeleton />;
  if (status === "anon") return null; // redirect in flight

  const handleLogout = async () => {
    try {
      await logoutSession();
    } catch {
      toast.error("Logout request failed, but you've been signed out locally.");
    }
  };

  return (
    <div className="flex min-h-screen flex-1">
      <aside className="hidden w-56 shrink-0 flex-col gap-7 border-r bg-sidebar px-3 py-5 md:flex">
        <Link
          href="/organizations"
          className="font-display px-2.5 text-[0.9375rem] leading-none focus-visible:ring-2
            focus-visible:ring-ring/50 focus-visible:outline-none"
        >
          Sapan<span className="text-signal">jai</span>
        </Link>
        <nav className="flex flex-col gap-6">
          <NavLinks />
        </nav>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center justify-between gap-4 border-b px-4 py-2.5 sm:px-6">
          <ScopeChain email={user?.email ?? ""} />
          <div className="flex shrink-0 items-center gap-1">
            <ThemeToggle />
            <Button variant="ghost" size="sm" onClick={() => void handleLogout()}>
              Log out
            </Button>
          </div>
        </header>

        <nav className="flex gap-1 overflow-x-auto border-b px-3 py-2 md:hidden">
          <NavLinks className="contents" />
        </nav>

        <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-7 sm:px-6 sm:py-9">{children}</main>
      </div>
    </div>
  );
}
