"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { logout as logoutRequest, refresh as refreshRequest } from "@/lib/api/endpoints";

import { clearTokens, getRefreshToken, setTokens, type TokenPair } from "./token-store";

export type SessionStatus = "loading" | "authed" | "anon";

export interface SessionUser {
  id: string;
  email: string;
}

interface SessionState {
  status: SessionStatus;
  user: SessionUser | null;
}

interface SessionContextValue extends SessionState {
  // Called by the login/register pages once they have a fresh token pair.
  applyTokens: (tokens: TokenPair) => void;
  logoutSession: () => Promise<void>;
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

export function SessionProvider({ children }: { children: React.ReactNode }) {
  // Always starts at "loading", server and client alike — reading
  // localStorage during the lazy initializer would differ between the
  // server render (no `window`) and the client's hydration render (`window`
  // exists there from the start), causing a hydration mismatch. The real
  // token check below only ever runs after mount, inside the effect.
  const [state, setState] = useState<SessionState>({ status: "loading", user: null });

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
    }
  }, [applyAccessToken]);

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

  const value = useMemo(
    () => ({ status: state.status, user: state.user, applyTokens, logoutSession }),
    [state, applyTokens, logoutSession],
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
