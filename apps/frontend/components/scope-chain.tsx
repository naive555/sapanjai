"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { CheckIcon, ChevronsUpDownIcon, PlusIcon } from "lucide-react";

import { RoleBadge } from "@/components/role-badge";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { listOrganizations } from "@/lib/api/endpoints";
import { useActiveOrgId, useSelectOrg } from "@/lib/org/active-org";

/**
 * The scope chain — identity, tenant, authority — is the credentials every
 * request carries, made visible: it is the backend's RequireAuth → RequireOrg
 * → RequirePermission middleware chain, in order.
 *
 * It exists because the one genuinely expensive mistake in a multi-tenant
 * console is acting on the wrong tenant, or assuming more authority than you
 * hold. The org segment is also the switcher, so the thing that tells you
 * where you are is the same thing that moves you.
 */
export function ScopeChain({ email }: { email: string }) {
  const { data: memberships } = useQuery({ queryKey: ["organizations"], queryFn: listOrganizations });
  const activeOrgId = useActiveOrgId();
  const selectOrg = useSelectOrg();

  const active = memberships?.find((m) => m.organizationId === activeOrgId);
  // Three distinct states, not two. A tenant can be selected while its
  // membership record is still in flight — reporting that as "unscoped"
  // would contradict the nav (which enables org routes off the id alone)
  // and would be a lie about scope in the one element meant to be
  // authoritative about it.
  const resolving = activeOrgId !== null && active === undefined;
  const connected = active !== undefined;

  // Restart the sweep whenever the tenant changes — but never on first paint,
  // where nothing has actually changed yet.
  const previousOrgId = useRef<string | null | undefined>(undefined);
  const [sweep, setSweep] = useState(0);
  useEffect(() => {
    if (previousOrgId.current !== undefined && previousOrgId.current !== activeOrgId) {
      setSweep((n) => n + 1);
    }
    previousOrgId.current = activeOrgId;
  }, [activeOrgId]);

  return (
    <div
      key={sweep}
      className={`flex min-w-0 items-center gap-2 rounded-md ${sweep > 0 ? "scope-sweep" : ""}`}
    >
      {/* Only the login/register responses carry an `email` claim — a token
          minted by /auth/refresh is `sub` only, by contract (docs/02), so
          after any full reload the identity is genuinely unknown. Drop the
          segment rather than render an empty one: tenant and authority are
          the two that prevent mistakes, and a dangling separator would
          imply a value that failed to load. */}
      {email && (
        <>
          <span
            className="hidden truncate font-mono text-xs text-muted-foreground sm:inline"
            title={email}
          >
            {email}
          </span>
          <Separator connected={connected} />
        </>
      )}

      <DropdownMenu>
        <DropdownMenuTrigger
          className="flex min-w-0 items-center gap-1.5 rounded-sm px-1.5 py-1 font-mono text-xs font-medium
            text-foreground transition-colors outline-none hover:bg-accent focus-visible:ring-2
            focus-visible:ring-ring/50 aria-expanded:bg-accent"
        >
          <span className={`truncate ${resolving ? "text-muted-foreground" : ""}`}>
            {active?.organization.slug ?? (resolving ? "resolving…" : "no organization")}
          </span>
          <ChevronsUpDownIcon className="size-3 shrink-0 text-muted-foreground" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="min-w-56">
          <DropdownMenuGroup>
            <DropdownMenuLabel className="label-eyebrow">Switch tenant</DropdownMenuLabel>
            {memberships?.length ? (
              memberships.map((m) => (
                <DropdownMenuItem
                  key={m.organizationId}
                  onClick={() => selectOrg(m.organizationId)}
                  className="gap-3"
                >
                  <span className="flex min-w-0 flex-1 flex-col">
                    <span className="truncate text-sm">{m.organization.name}</span>
                    <span className="truncate font-mono text-[0.6875rem] text-muted-foreground">
                      {m.organization.slug}
                    </span>
                  </span>
                  <RoleBadge role={m.role} />
                  <CheckIcon
                    className={`size-3.5 shrink-0 ${
                      m.organizationId === activeOrgId ? "text-signal" : "invisible"
                    }`}
                  />
                </DropdownMenuItem>
              ))
            ) : (
              <DropdownMenuItem disabled>No organizations yet</DropdownMenuItem>
            )}
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem render={<Link href="/organizations" />}>
            <PlusIcon className="size-4" /> Create organization
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <Separator connected={connected} />

      {active ? (
        <RoleBadge role={active.role} />
      ) : (
        <span className="font-mono text-[0.6875rem] tracking-[0.08em] text-muted-foreground/60 uppercase">
          {resolving ? "…" : "unscoped"}
        </span>
      )}
    </div>
  );
}

// A solid connector means the scope resolves; a dashed one means the chain is
// broken and the org-scoped routes will refuse the request.
function Separator({ connected }: { connected: boolean }) {
  return (
    <span
      aria-hidden
      className={`h-px w-3 shrink-0 ${
        connected ? "bg-border" : "bg-transparent [border-top:1px_dashed_var(--border)]"
      }`}
    />
  );
}
