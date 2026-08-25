"use client";

import { useState } from "react";
import Link from "next/link";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BookOpenIcon, PlusIcon } from "lucide-react";
import { Controller, useForm, useWatch, type Control } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";

import {
  GoogleSheetsFormFields,
  googleSheetsConfigFieldsSchema,
  refineGoogleSheetsConfig,
  toGoogleSheetsConfig,
  type GoogleSheetsFieldValues,
} from "@/components/connectors/google-sheets-form";
import { ConnectorStatus } from "@/components/connector-status";
import { CopyableId } from "@/components/copyable-id";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { ApiError } from "@/lib/api/client";
import {
  createConnector,
  deleteConnector,
  healthCheckConnector,
  listConnectors,
  type ConnectorResponse,
} from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

// Mirrors internal/module/connector/dto.go's ParseConfig-adjacent JSON
// validation for a "generic" connector: the backend only requires a
// non-empty object (it does no shape validation for this type), so the only
// thing worth rejecting client-side is malformed or empty JSON.
function validateGenericConfigJson(text: string | undefined, ctx: z.RefinementCtx) {
  const trimmed = (text ?? "").trim();
  if (!trimmed) {
    ctx.addIssue({ code: "custom", path: ["configJson"], message: "Config JSON is required." });
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    ctx.addIssue({ code: "custom", path: ["configJson"], message: "Not valid JSON." });
    return;
  }
  if (
    typeof parsed !== "object" ||
    parsed === null ||
    Array.isArray(parsed) ||
    Object.keys(parsed as Record<string, unknown>).length === 0
  ) {
    ctx.addIssue({ code: "custom", path: ["configJson"], message: "Config must be a non-empty JSON object." });
  }
}

// name + type live at this level; the google_sheets fields are merged in
// from google-sheets-form.tsx rather than redeclared, so the create dialog
// and the edit page validate those fields through the exact same rule.
const createConnectorSchema = z
  .object({
    name: z.string().min(1, "Name is required").max(100, "Name must be 100 characters or fewer"),
    type: z.enum(["generic", "google_sheets"]),
    configJson: z.string().optional(),
  })
  .merge(googleSheetsConfigFieldsSchema)
  .superRefine((values, ctx) => {
    if (values.type === "google_sheets") {
      refineGoogleSheetsConfig(values, ctx);
    } else {
      validateGenericConfigJson(values.configJson, ctx);
    }
  });
type CreateConnectorValues = z.infer<typeof createConnectorSchema>;

const createDefaults: CreateConnectorValues = {
  name: "",
  type: "generic",
  configJson: "",
  clientId: "",
  clientSecret: "",
  refreshToken: "",
  spreadsheetIdsText: "",
  driveFolderIdsText: "",
  headerRowsText: "",
};

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "—";
}

function typeLabel(type: string): string {
  return type === "google_sheets" ? "Google Sheets" : "Generic";
}

export default function ConnectorsPage() {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<{ id: string; name: string } | null>(null);

  const {
    data: connectors,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["connectors", activeOrgId],
    queryFn: listConnectors,
    enabled: activeOrgId !== null,
    // A member without connector:read gets a deterministic 403 — spinning
    // through retries would just delay the denied state the table needs.
    retry: false,
  });

  const invalidateConnectors = () => queryClient.invalidateQueries({ queryKey: ["connectors", activeOrgId] });

  // ---- create connector ----
  const createForm = useForm<CreateConnectorValues>({
    resolver: zodResolver(createConnectorSchema),
    defaultValues: createDefaults,
  });
  // useWatch (not the form's own .watch()) — the latter returns a function
  // React Compiler can't safely memoize, per the lint warning it trips.
  const selectedType = useWatch({ control: createForm.control, name: "type" });

  const createMutation = useMutation({
    // See the note on the edit page's updateMutation: `variables` holds the
    // plaintext config, and reset() only clears the hook's copy unless the
    // cache entry is dropped with it.
    gcTime: 0,
    mutationFn: (values: CreateConnectorValues) => {
      const config =
        values.type === "google_sheets"
          ? toGoogleSheetsConfig(values)
          : (JSON.parse(values.configJson!.trim()) as Record<string, unknown>);
      return createConnector({ name: values.name, type: values.type, config });
    },
    onSuccess: (created) => {
      invalidateConnectors();
      toast.success(`Connector "${created.name}" created.`);
      createForm.reset(createDefaults);
      setCreateOpen(false);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to create connector.");
    },
  });

  // ---- health check ----
  const healthCheckMutation = useMutation({
    mutationFn: (connectorId: string) => healthCheckConnector(connectorId),
    onSuccess: (updated) => {
      queryClient.setQueryData<ConnectorResponse[]>(["connectors", activeOrgId], (prev) =>
        prev?.map((c) => (c.id === updated.id ? updated : c)),
      );
      toast.success(`Health check complete — status: ${updated.status}.`);
    },
    onError: (err) => {
      // "generic" has no adapter registered at all — every check against one
      // is a 501, by contract, not a probe that happened to fail.
      if (err instanceof ApiError && err.status === 501) {
        toast.error("This connector type has no health check yet.");
      } else {
        toast.error(
          err instanceof ApiError
            ? "Health check failed — the probe's own error is never returned; check the config."
            : "Failed to run health check.",
        );
      }
    },
  });

  // ---- delete connector ----
  const deleteMutation = useMutation({
    mutationFn: deleteConnector,
    onSuccess: () => {
      invalidateConnectors();
      toast.success("Connector deleted.");
      setDeleteTarget(null);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to delete connector.");
      setDeleteTarget(null);
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="connectors"
        description="The systems an agent can reach through this organization."
      >
        <Button variant="ghost" size="sm" render={<Link href="/connectors/google-sheets-setup" />}>
          <BookOpenIcon className="size-4" /> Setup guide
        </Button>
        <Dialog
          open={createOpen}
          onOpenChange={(open) => {
            setCreateOpen(open);
            if (!open) createForm.reset(createDefaults);
          }}
        >
          <DialogTrigger render={<Button size="sm" />}>
            <PlusIcon className="size-4" /> Create connector
          </DialogTrigger>
          <DialogContent className="sm:max-w-lg">
            <DialogHeader>
              <DialogTitle>Create connector</DialogTitle>
              <DialogDescription>
                Config is required now — a connector can&apos;t exist without its credentials, so there&apos;s no
                &ldquo;create empty, configure later&rdquo; path. New connectors start &ldquo;inactive&rdquo; until
                a health check succeeds.
              </DialogDescription>
            </DialogHeader>
            <form
              onSubmit={createForm.handleSubmit(async (values) => {
                try {
                  await createMutation.mutateAsync(values);
                } catch {
                  // Reported by the mutation's own onError toast.
                } finally {
                  // Drops the plaintext config out of the mutation's cached
                  // `variables`, which otherwise keeps a pasted OAuth secret
                  // in memory for as long as this page stays mounted.
                  createMutation.reset();
                }
              })}
              noValidate
              className="flex max-h-[65vh] flex-col gap-5 overflow-y-auto"
            >
              <FieldGroup>
                <Field data-invalid={!!createForm.formState.errors.name}>
                  <FieldLabel htmlFor="connector-name">Name</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="name"
                    render={({ field }) => <Input id="connector-name" placeholder="Acme billing sheet" {...field} />}
                  />
                  <FieldError errors={[createForm.formState.errors.name]} />
                </Field>

                <Field data-invalid={!!createForm.formState.errors.type}>
                  <FieldLabel htmlFor="connector-type">Type</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="type"
                    render={({ field }) => (
                      <Select value={field.value} onValueChange={field.onChange}>
                        <SelectTrigger id="connector-type" className="w-full">
                          <SelectValue>{(value: string) => typeLabel(value)}</SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="generic">Generic</SelectItem>
                          <SelectItem value="google_sheets">Google Sheets</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <FieldError errors={[createForm.formState.errors.type]} />
                </Field>
              </FieldGroup>

              {selectedType === "google_sheets" ? (
                <GoogleSheetsFormFields
                  mode="create"
                  // Same reasoning as the field-shape note in
                  // google-sheets-form.tsx: this form's Control carries more
                  // fields (name, type, configJson) than the google-sheets
                  // fields component needs, and react-hook-form's Control<T>
                  // isn't cleanly polymorphic across that superset, so it's
                  // narrowed explicitly here rather than made generic there.
                  control={createForm.control as unknown as Control<GoogleSheetsFieldValues>}
                  errors={createForm.formState.errors}
                />
              ) : (
                <FieldGroup>
                  <Field data-invalid={!!createForm.formState.errors.configJson}>
                    <FieldLabel htmlFor="connector-config-json">Config (JSON)</FieldLabel>
                    <Controller
                      control={createForm.control}
                      name="configJson"
                      render={({ field }) => (
                        <Textarea
                          id="connector-config-json"
                          rows={6}
                          className="font-mono text-sm"
                          placeholder={'{\n  "apiKey": "..."\n}'}
                          {...field}
                        />
                      )}
                    />
                    <FieldDescription>
                      The &ldquo;generic&rdquo; type has no adapter yet (health checks always return unsupported)
                      — this is stored sealed at rest, as-is.
                    </FieldDescription>
                    <FieldError errors={[createForm.formState.errors.configJson]} />
                  </Field>
                </FieldGroup>
              )}

              <DialogFooter>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? "Creating…" : "Create"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </PageHeader>

      <DataTable
        columns={["Name", "ID", "Type", "Status", "Last health check", "Created", { label: "", align: "end" }]}
      >
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={7}>Loading…</TableMessage>
          ) : isError ? (
            <TableMessage colSpan={7}>
              {error instanceof ApiError && error.status === 403
                ? "You don't have permission to view connectors in this organization."
                : "Failed to load connectors."}
            </TableMessage>
          ) : connectors?.length ? (
            connectors.map((connector) => {
              const checking = healthCheckMutation.isPending && healthCheckMutation.variables === connector.id;

              return (
                <TableRow key={connector.id}>
                  <TableCell className="font-medium">
                    <Link href={`/connectors/${connector.id}`} className="underline-offset-4 hover:underline">
                      {connector.name}
                    </Link>
                  </TableCell>
                  {/*
                    * The id is the one field an MCP client actually needs —
                    * POST /mcp/:connectorId — and before this column the only
                    * way to obtain it was to open a google_sheets connector
                    * and read the address bar, which left generic connectors
                    * with no route to their own id at all.
                    */}
                  <TableCell>
                    <CopyableId value={connector.id} label="connector ID" />
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{typeLabel(connector.type)}</TableCell>
                  <TableCell>
                    <ConnectorStatus status={connector.status} />
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {formatDate(connector.lastHealthCheckAt)}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {formatDate(connector.createdAt)}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="outline"
                        size="xs"
                        disabled={checking}
                        onClick={() => healthCheckMutation.mutate(connector.id)}
                      >
                        {checking ? "Checking…" : "Run health check"}
                      </Button>
                      <Button
                        variant="ghost"
                        size="xs"
                        className="text-muted-foreground hover:text-destructive"
                        onClick={() => setDeleteTarget({ id: connector.id, name: connector.name })}
                      >
                        Delete
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })
          ) : (
            <TableMessage colSpan={7}>
              No connectors yet. Create one to give the MCP gateway an upstream to reach — or{" "}
              <Link
                href="/connectors/google-sheets-setup"
                className="text-foreground underline underline-offset-4 hover:text-signal"
              >
                walk through the Google setup
              </Link>{" "}
              first if you don&apos;t have credentials yet.
            </TableMessage>
          )}
        </TableBody>
      </DataTable>

      <Dialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete connector</DialogTitle>
            <DialogDescription>
              Delete <span className="font-mono text-foreground">{deleteTarget?.name}</span>? Any MCP key using
              it loses access to this connector immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleteMutation.isPending}
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget.id)}
            >
              {deleteMutation.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
