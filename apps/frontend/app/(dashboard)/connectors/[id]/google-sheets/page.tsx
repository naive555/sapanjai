"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon } from "lucide-react";
import { toast } from "sonner";

import { GoogleSheetsForm } from "@/components/connectors/google-sheets-form";
import { ConnectorStatus } from "@/components/connector-status";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import { getConnector, updateConnector } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "never";
}

export default function GoogleSheetsConnectorPage() {
  const { id } = useParams<{ id: string }>();
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();

  const {
    data: connector,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["connectors", activeOrgId, id],
    queryFn: () => getConnector(id),
    enabled: activeOrgId !== null && !!id,
    // A wrong-org / nonexistent id is a deterministic 404 — render the
    // not-found state immediately rather than retrying it.
    retry: false,
  });

  const updateMutation = useMutation({
    mutationFn: (config: Record<string, unknown>) => updateConnector(id, { config }),
    // The mutation's `variables` are the plaintext OAuth credential. reset()
    // alone only clears the hook's view of them — the MutationCache keeps the
    // entry for gcTime (5 minutes by default), so the secret would outlive
    // the save it belonged to. gcTime: 0 drops the entry the moment reset()
    // releases the last observer.
    gcTime: 0,
    onSuccess: (updated) => {
      queryClient.setQueryData(["connectors", activeOrgId, id], updated);
      queryClient.invalidateQueries({ queryKey: ["connectors", activeOrgId] });
      toast.success("Configuration saved.");
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to save configuration.");
    },
  });

  return (
    <div className="flex max-w-2xl flex-col gap-6">
      <PageHeader
        title="google sheets connector"
        description="Replace this connector's OAuth credentials and spreadsheet/Drive allowlist."
      >
        <Button variant="outline" size="sm" render={<Link href="/connectors" />}>
          <ArrowLeftIcon className="size-4" /> Back to connectors
        </Button>
      </PageHeader>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : isError || !connector ? (
        <div className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError && error.status === 404
            ? "This connector doesn't exist, or belongs to a different organization."
            : "Failed to load this connector."}
        </div>
      ) : connector.type !== "google_sheets" ? (
        <div className="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
          <span className="font-medium text-foreground">{connector.name}</span> is a{" "}
          <span className="font-mono">{connector.type}</span> connector, not a{" "}
          <span className="font-mono">google_sheets</span> one — this page only edits Google Sheets
          configuration.
        </div>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
            <span className="font-medium">{connector.name}</span>
            <ConnectorStatus status={connector.status} />
            <span className="text-xs text-muted-foreground">
              Last health check: {formatDate(connector.lastHealthCheckAt)}
            </span>
          </div>

          <GoogleSheetsForm
            onSubmit={async (config) => {
              try {
                // Awaited (not fire-and-forget) so the form knows to clear
                // its fields only when the save actually succeeded.
                await updateMutation.mutateAsync(config);
              } finally {
                // Clears the plaintext credential out of the mutation's
                // cached `variables`, which otherwise outlives the save on a
                // page the user usually stays on afterwards.
                updateMutation.reset();
              }
            }}
            submitting={updateMutation.isPending}
            submitLabel="Save configuration"
          />
        </>
      )}
    </div>
  );
}
