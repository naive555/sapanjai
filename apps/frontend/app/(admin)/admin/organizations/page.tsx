"use client";

import { useState } from "react";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";

import { Pager } from "@/components/admin/pager";
import { CopyableId } from "@/components/copyable-id";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Input } from "@/components/ui/input";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { listAdminOrganizations } from "@/lib/api/endpoints";

const PAGE_SIZE = 50;

function formatDate(value: string): string {
  return new Date(value).toISOString().slice(0, 10);
}

export default function AdminOrganizationsPage() {
  const [search, setSearch] = useState("");
  const [offset, setOffset] = useState(0);

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "organizations", { search, offset }],
    queryFn: () => listAdminOrganizations({ search: search || undefined, limit: PAGE_SIZE, offset }),
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="organizations" description="Every tenant on the platform, searchable by name or slug.">
        <Input
          aria-label="Search organizations"
          placeholder="Search name or slug…"
          className="w-64"
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
            setOffset(0);
          }}
        />
      </PageHeader>

      <DataTable columns={["Name", "Slug", "Plan", "Members", "Connectors", "MCP keys", "Created"]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={7}>Loading…</TableMessage>
          ) : isError ? (
            <TableMessage colSpan={7}>
              {error instanceof ApiError ? error.message : "Failed to load organizations."}
            </TableMessage>
          ) : data?.items.length ? (
            data.items.map((org) => (
              <TableRow key={org.id}>
                <TableCell className="font-medium">
                  <Link href={`/admin/organizations/${org.id}`} className="underline-offset-4 hover:underline">
                    {org.name}
                  </Link>
                  <CopyableId value={org.id} label="organization ID" className="mt-0.5 block" />
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{org.slug}</TableCell>
                <TableCell className="text-sm">{org.planName ?? "—"}</TableCell>
                <TableCell className="text-sm text-muted-foreground">{org.memberCount}</TableCell>
                <TableCell className="text-sm text-muted-foreground">{org.connectorCount}</TableCell>
                <TableCell className="text-sm text-muted-foreground">{org.mcpKeyCount}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{formatDate(org.createdAt)}</TableCell>
              </TableRow>
            ))
          ) : (
            <TableMessage colSpan={7}>
              {search ? "No organizations match this search." : "No organizations yet."}
            </TableMessage>
          )}
        </TableBody>
      </DataTable>

      {data && data.total > 0 && (
        <Pager offset={offset} limit={PAGE_SIZE} total={data.total} onOffsetChange={setOffset} />
      )}
    </div>
  );
}
