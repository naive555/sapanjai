"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { exitImpersonation as exitImpersonationToken, subscribeImpersonationExpired } from "@/lib/api/client";
import {
  logout as logoutRequest,
  me,
  refresh as refreshRequest,
  type AdminImpersonateResponse,
} from "@/lib/api/endpoints";

import {
  beginImpersonation,
  clearTokens,
  getAccessToken,
  getRefreshToken,
  isImpersonating,
  setTokens,
  subscribeImpersonation,
  type TokenPair,
} from "./token-store";

export type SessionStatus = "loading" | "authed" | "anon";

export interface SessionUser {
  id: string;
  email: string;
}

// Set only while impersonating — the JWT itself only carries id/email
// (decodeUser below), so displayName and the countdown deadline are kept
// here instead, populated once at POST /admin/users/:userId/impersonate's
// response and cleared on exit/expiry.
export interface ImpersonationTarget {
  userId: string;
  email: string;
  displayName: string | null;
  /** Date.now()-comparable epoch ms, computed from the response's expiresIn. */
  expiresAt: number;
}

interface SessionState {
  status: SessionStatus;
  user: SessionUser | null;
}

interface SessionContextValue extends SessionState {
  // Called by the login/register pages once they have a fresh token pair.
  applyTokens: (tokens: TokenPair) => void;
  logoutSession: () => Promise<void>;
  // Read fresh from GET /auth/me (never a JWT claim — see
  // docs/11-admin-panel.md D1) rather than decoded off the token, which is
  // what makes the nav's "Admin" entry disappear immediately on demotion
  // instead of surviving a stale access token's lifetime.
  platformRole: "superadmin" | "support" | null;
  isPlatformStaff: boolean;
  impersonating: boolean;
  impersonationTarget: ImpersonationTarget | null;
  // Called by the admin console's "Impersonate" action once it has a fresh
  // POST /admin/users/:userId/impersonate response.
  startImpersonation: (response: AdminImpersonateResponse) => void;
  // Called by the impersonation banner's "Exit" button.
  exitImpersonation: () => Promise<void>;
}

const SessionContext = createContext<SessionContextValue | null>(null);

function base64UrlDecode(input: string): string {
  const base64 = input.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), "=");
  return atob(padded);
}

// Decodes the access token's payload client-side for display purposes only
// (no signature verification — the backend is the source of truth for auth).
function decodeUser(accessToken: string): SessionUser | null {
  try {
    const payload = accessToken.split(".")[1];
    if (!payload) return null;
    const claims = JSON.parse(base64UrlDecode(payload)) as { sub?: string; email?: string };
    if (!claims.sub) return null;
    return { id: claims.sub, email: claims.email ?? "" };
  } catch {
    return null;
  }
}

function stateFromAccessToken(accessToken: string | null): SessionState {
  if (!accessToken) return { status: "anon", user: null };
  const decoded = decodeUser(accessToken);
  return decoded ? { status: "authed", user: decoded } : { status: "anon", user: null };
}

function getImpersonatingServerSnapshot(): boolean {
  return false;
}

export function SessionProvider({ children }: { children: React.ReactNode }) {
  // Always starts at "loading", server and client alike — reading
  // localStorage during the lazy initializer would differ between the
  // server render (no `window`) and the client's hydration render (`window`
  // exists there from the start), causing a hydration mismatch. The real
  // token check below only ever runs after mount, inside the effect.
  const [state, setState] = useState<SessionState>({ status: "loading", user: null });
  const [impersonationTarget, setImpersonationTarget] = useState<ImpersonationTarget | null>(null);
  const queryClient = useQueryClient();

  // Subscribes directly to token-store's pub-sub, same pattern
  // lib/org/active-org.ts's useActiveOrgId uses — every code path that
  // starts or ends impersonation (startImpersonation/exitImpersonation
  // below, clearTokens on logout, client.ts's own 401-expiry path) already
  // goes through token-store, so this stays correct with no separate state
  // to keep in sync.
  const impersonating = useSyncExternalStore(subscribeImpersonation, isImpersonating, getImpersonatingServerSnapshot);

  const applyAccessToken = useCallback((accessToken: string | null) => {
    setState(stateFromAccessToken(accessToken));
  }, []);

  const applyTokens = useCallback(
    (tokens: TokenPair) => {
      setTokens(tokens);
      applyAccessToken(tokens.accessToken);
    },
    [applyAccessToken],
  );

  const logoutSession = useCallback(async () => {
    const refreshToken = getRefreshToken();
    try {
      if (refreshToken) {
        await logoutRequest({ refreshToken });
      }
    } finally {
      clearTokens();
      applyAccessToken(null);
      setImpersonationTarget(null);
    }
  }, [applyAccessToken]);

  // GET /admin/users/:userId/impersonate's caller (the admin console) hands
  // the response straight here. This deliberately does NOT go through
  // applyTokens/setTokens: the whole point of the impersonation client
  // model (docs/11-admin-panel.md §5, execution plan Task 5.2) is that
  // nothing new is persisted to localStorage — beginImpersonation only ever
  // touches the in-memory access token, so closing the tab ends the
  // impersonation by itself.
  const startImpersonation = useCallback(
    (response: AdminImpersonateResponse) => {
      beginImpersonation(response.accessToken);
      setImpersonationTarget({
        userId: response.user.id,
        email: response.user.email,
        displayName: response.user.displayName,
        expiresAt: Date.now() + response.expiresIn * 1000,
      });
      applyAccessToken(response.accessToken);
      // Every org-scoped query in flight was answered under the ADMIN's
      // own identity (or no identity at all) — none of that is valid for
      // the target's data, so the whole cache needs to go, the same way
      // useSelectOrg's org switch invalidates everything.
      void queryClient.invalidateQueries();
    },
    [applyAccessToken, queryClient],
  );

  // Shared tail end of both a deliberate "Exit" click and an automatic
  // 401-driven expiry (the useEffect below) — re-derives session state from
  // whatever token client.ts's restoreAdminSession left in memory (the
  // restored admin token, or null if even that refresh failed).
  const settleAfterImpersonation = useCallback(() => {
    setImpersonationTarget(null);
    applyAccessToken(getAccessToken());
    void queryClient.invalidateQueries();
  }, [applyAccessToken, queryClient]);

  const exitImpersonation = useCallback(async () => {
    await exitImpersonationToken();
    settleAfterImpersonation();
  }, [settleAfterImpersonation]);

  // THE dangerous-bug guard from execution plan Task 5.2, closed on this
  // side: client.ts's apiRequest refuses to silently retry a request under
  // a freshly-restored admin token when the failing request was itself
  // impersonation-authenticated (see its own long comment). It still has to
  // restore *something* — the app can't be left with a dead in-memory token
  // — so it does the restore itself and then just announces "this
  // happened" here, where there's a toast and a query cache to invalidate.
  useEffect(() => {
    return subscribeImpersonationExpired(() => {
      settleAfterImpersonation();
      toast.error("Impersonation session expired — you're back in your own account.");
    });
  }, [settleAfterImpersonation]);

  useEffect(() => {
    let cancelled = false;

    // Deferred through a resolved promise so every setState call below runs
    // inside an async continuation rather than synchronously in the effect
    // body (see client.ts's refresh flow for the same pattern).
    Promise.resolve(getRefreshToken()).then((refreshToken) => {
      if (cancelled) return;
      if (!refreshToken) {
        applyAccessToken(null);
        return;
      }
      refreshRequest({ refreshToken })
        .then((tokens) => {
          if (cancelled) return;
          setTokens(tokens);
          applyAccessToken(tokens.accessToken);
        })
        .catch(() => {
          if (cancelled) return;
          clearTokens();
          applyAccessToken(null);
        });
    });

    return () => {
      cancelled = true;
    };
  }, [applyAccessToken]);

  // Deliberately its own query rather than folded into the token-derived
  // state above: platformRole is never a JWT claim (docs/11-admin-panel.md
  // D1 — the same reasoning CLAUDE.md already records for isVerified), so
  // the only way to know it is to ask GET /auth/me. Shares its cache with
  // VerificationBanner's identical ["me"] query.
  const { data: meData } = useQuery({
    queryKey: ["me"],
    queryFn: me,
    enabled: state.status === "authed",
  });
  const platformRole = meData?.platformRole ?? null;

  const value = useMemo(
    () => ({
      status: state.status,
      user: state.user,
      applyTokens,
      logoutSession,
      platformRole,
      isPlatformStaff: platformRole !== null,
      impersonating,
      impersonationTarget,
      startImpersonation,
      exitImpersonation,
    }),
    [
      state,
      applyTokens,
      logoutSession,
      platformRole,
      impersonating,
      impersonationTarget,
      startImpersonation,
      exitImpersonation,
    ],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) {
    throw new Error("useSession must be used within a SessionProvider");
  }
  return ctx;
}
