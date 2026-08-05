"use client";

import { useCallback, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { getActiveOrgId, setActiveOrgId, subscribeActiveOrg } from "@/lib/auth/token-store";

function getServerSnapshot(): string | null {
  return null;
}

// Subscribes directly to the token-store's pub-sub rather than mirroring it
// into a Context/Provider — every code path that changes the active org
// (explicit selection, logout, a failed background refresh) already goes
// through token-store, so this stays correct with no separate state to sync.
export function useActiveOrgId(): string | null {
  return useSyncExternalStore(subscribeActiveOrg, getActiveOrgId, getServerSnapshot);
}

// Selecting an org invalidates every org-scoped query, since the
// x-organization-id header the API client sends is about to change.
export function useSelectOrg(): (orgId: string | null) => void {
  const queryClient = useQueryClient();

  return useCallback(
    (orgId: string | null) => {
      setActiveOrgId(orgId);
      void queryClient.invalidateQueries();
    },
    [queryClient],
  );
}
