import type { Metadata } from "next";
import Link from "next/link";
import { ArrowLeftIcon, ChevronDownIcon } from "lucide-react";

import { Callout } from "@/components/callout";
import { CopyableCode } from "@/components/copyable-code";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";

/*
 * The one long-form page in the dashboard, and the only customer-facing
 * onboarding text that doesn't fit beside a form field.
 *
 * It lives here rather than in docs/ because the reader is a customer admin
 * who will never clone this repo — and it lives ONLY here, so there is one
 * version-controlled copy to keep true rather than two that drift.
 *
 * Routing note: this sits alongside `connectors/[id]/`. A static segment wins
 * over a sibling dynamic one in the App Router, so `/connectors/google-sheets-setup`
 * resolves here and never to `[id]` — but that also means "google-sheets-setup"
 * is now a reserved connector-id-shaped path. Connector ids are UUIDs, so
 * there is no collision to worry about.
 *
 * A server component on purpose: it is static prose with no session or org
 * dependency, so it renders once at build time. The client bits (copy
 * buttons, the collapsed OAuth section) are islands inside it.
 *
 * Service account is the primary path here, matching the form's default
 * (components/connectors/google-sheets-form.tsx): the adapter requests
 * drive.readonly, a Google *restricted* scope, so a customer's own OAuth
 * client cannot leave "Testing" publishing status without a paid annual CASA
 * assessment, and Google expires every refresh token a Testing app issues
 * after 7 days. A service account has no consent screen and no refresh token
 * to expire — the customer just shares each file with its address. The OAuth
 * walkthrough stays below, collapsed, for the one case a service account
 * can't cover: a Workspace admin who has disabled sharing outside the org.
 */

export const metadata: Metadata = {
  title: "Connect a Google Sheet — Sapanjai",
  description:
    "Set up the Google credentials and allowlist a google_sheets connector needs, then prove they work.",
};

const SHEETS_SCOPE = "https://www.googleapis.com/auth/spreadsheets.readonly";
const DRIVE_SCOPE = "https://www.googleapis.com/auth/drive.readonly";

// Pre-encoded for the authorization URL below: the two scopes above, space
// separated, percent-encoded. Written out rather than computed so that what
// the reader copies is exactly what is reviewed here.
const AUTH_URL =
  "https://accounts.google.com/o/oauth2/v2/auth" +
  "?client_id=YOUR_CLIENT_ID" +
  "&redirect_uri=http://localhost:8080" +
  "&response_type=code" +
  "&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fspreadsheets.readonly%20https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fdrive.readonly" +
  "&access_type=offline" +
  "&prompt=consent";

const TOKEN_EXCHANGE = `curl -s https://oauth2.googleapis.com/token \\
  -d client_id=YOUR_CLIENT_ID \\
  -d client_secret=YOUR_CLIENT_SECRET \\
  -d code=THE_CODE_FROM_THE_REDIRECT \\
  -d redirect_uri=http://localhost:8080 \\
  -d grant_type=authorization_code`;

function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }) {
  return (
    <section
      className="relative flex gap-4 pb-9 last:pb-0 after:absolute after:top-9 after:bottom-2 after:left-4
        after:w-px after:bg-border last:after:hidden"
    >
      <div
        className="relative z-10 flex size-8 shrink-0 items-center justify-center rounded-full border bg-card
          font-mono text-xs text-muted-foreground"
      >
        {n}
      </div>
      <div className="min-w-0 flex-1 space-y-3 pt-1">
        <h2 className="font-heading text-base leading-snug font-medium">{title}</h2>
        {children}
      </div>
    </section>
  );
}

function P({ children }: { children: React.ReactNode }) {
  return <p className="text-sm leading-relaxed text-muted-foreground">{children}</p>;
}

/**
 * A gap this page cannot honestly fill from here.
 *
 * Google's console UI has not been seen by whoever drafted this text, and
 * inventing a plausible button label is worse than admitting the gap: a
 * reader who can't find "Continue" on the screen in front of them assumes
 * they are on the wrong screen. Each of these marks a spot for someone with
 * the console actually open to replace with a real label or a screenshot.
 *
 * These are meant to be deleted, not to become permanent furniture — so this
 * deliberately spends no palette accent on them.
 */
function Todo({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex flex-wrap items-baseline gap-x-2 gap-y-1 rounded-md border border-dashed bg-muted/40 px-3 py-2 text-sm">
      <span className="label-eyebrow shrink-0">needs a screenshot</span>
      <span className="min-w-0 flex-1 text-muted-foreground">{children}</span>
    </p>
  );
}

// `start` matters wherever a numbered list is split around a code block: a
// second <ol> restarts at 1 by default, so step 4's "3." would silently
// render as "1." and a reader following along loses their place.
function Substeps({ start, children }: { start?: number; children: React.ReactNode }) {
  return (
    <ol
      start={start}
      className="ml-4 list-outside list-decimal space-y-2 text-sm leading-relaxed text-muted-foreground
        marker:font-mono marker:text-xs marker:text-muted-foreground/70"
    >
      {children}
    </ol>
  );
}

/**
 * A Google-side destination. Opened in a new tab on purpose: a reader is
 * halfway through a numbered list, and replacing the list with the console
 * loses their place in it.
 */
function ExtLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="text-foreground underline underline-offset-4 hover:text-signal"
    >
      {children}
    </a>
  );
}

/** An inline literal — an id, a URL fragment, a form field name. */
function C({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-[0.8125rem] text-foreground">{children}</code>;
}

/**
 * A collapsed, secondary path. The repo has no accordion primitive, so this
 * is a plain `<details>` styled to match the numbered-step cards around it —
 * native semantics (keyboard toggling, no JS state) beat reaching for a new
 * dependency for one collapsible section.
 *
 * Closed by default: the whole point of demoting OAuth below the fold is
 * that a reader who doesn't need it never has to scroll past its contents.
 */
function Disclosure({ summary, children }: { summary: string; children: React.ReactNode }) {
  return (
    <details className="group rounded-lg border bg-card p-5 open:pb-6">
      <summary className="flex cursor-pointer list-none items-center justify-between gap-3 font-heading text-base font-medium marker:content-none [&::-webkit-details-marker]:hidden">
        {summary}
        <ChevronDownIcon className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
      </summary>
      <div className="mt-5 space-y-5">{children}</div>
    </details>
  );
}

export default function GoogleSheetsSetupPage() {
  return (
    <div className="flex max-w-3xl flex-col gap-8">
      <PageHeader
        title="connect a google sheet"
        description="What Google's side needs before Sapanjai can read a spreadsheet. Done once per Google account, not once per sheet."
      >
        <Button variant="outline" size="sm" render={<Link href="/connectors" />}>
          <ArrowLeftIcon className="size-4" /> Back to connectors
        </Button>
      </PageHeader>

      <Callout title="Before you start: does your organization block external sharing?">
        The walkthrough below shares each spreadsheet with a service account — an address ending in{" "}
        <C>@…gserviceaccount.com</C>. If your Google Workspace admin has disabled sharing outside the
        organization, that block applies to a service account exactly as it would to any other outside
        account: Share will refuse the address or silently do nothing. If that&apos;s your situation, you
        need either a policy exception from your admin or the OAuth walkthrough collapsed at the bottom of
        this page — better to know that now than after setting up a service account that can&apos;t be
        shared with anything.
      </Callout>

      <div className="space-y-3 rounded-lg border bg-card p-5">
        <h2 className="font-heading text-base font-medium">What you&apos;re collecting</h2>
        <P>
          Two things. The first comes from Google and goes into the connector form as the credential; the
          second is the list of documents this connector is allowed to touch.
        </P>
        <ul className="space-y-1.5 text-sm text-muted-foreground">
          <li>a service account key (JSON) — steps 1&ndash;3</li>
          <li>spreadsheet IDs and/or Drive folder IDs — step 4</li>
        </ul>
        <Callout>
          Sapanjai has no &ldquo;Sign in with Google&rdquo; button yet, so there is no way to skip this by
          clicking through a consent screen in the dashboard. You do it once by hand in Google Cloud, paste
          the result in, and — unlike an OAuth refresh token — there is no clock running on it afterward.
        </Callout>
      </div>

      <div>
        <Step n={1} title="Create a Google Cloud project">
          <P>
            Go to <ExtLink href="https://console.cloud.google.com">console.cloud.google.com</ExtLink> and sign in as the Google account that can already open the
            spreadsheets you want to share. A project is just a container for the API access — one is enough
            for every sheet that account owns.
          </P>
          <Todo>
            The exact wording of the project picker and the &ldquo;new project&rdquo; control at the top of
            the console.
          </Todo>
          <Callout>
            If your organization uses Google Workspace, an admin may have to create the project for you, or
            allow you to. Worth checking before you spend twenty minutes on step 3.
          </Callout>
        </Step>

        <Step n={2} title="Enable the Sheets API and the Drive API">
          <P>
            Both, even if you only care about spreadsheets. Reading cells goes through the Sheets API, but
            listing what is in a folder and fetching an attachment go through Drive — and a connector with
            only Sheets enabled fails the moment an agent looks at a folder.
          </P>
          <P>
            These go straight to the right pages once a project is selected:{" "}
            <ExtLink href="https://console.cloud.google.com/apis/library/sheets.googleapis.com">
              enable the Sheets API
            </ExtLink>
            , then{" "}
            <ExtLink href="https://console.cloud.google.com/apis/library/drive.googleapis.com">
              enable the Drive API
            </ExtLink>
            .
          </P>
          <Todo>
            The label on the enable control, and how the page looks once an API is already enabled (so a
            reader can tell &ldquo;done&rdquo; from &ldquo;not started&rdquo;).
          </Todo>
        </Step>

        <Step n={3} title="Create a service account and download its JSON key">
          <P>
            A service account is Google&apos;s identity for a program rather than a person. It has an email
            address you&apos;ll share spreadsheets with in step 4, and Google hands it a JSON key file
            instead of asking anyone to sign in — there is no consent screen and nothing here for Google to
            expire after seven days.
          </P>
          <Substeps>
            <li>
              In the project from step 1, go to{" "}
              <ExtLink href="https://console.cloud.google.com/iam-admin/serviceaccounts">
                IAM &amp; Admin → Service accounts
              </ExtLink>{" "}
              and create one. Any name works; you&apos;re the only one who&apos;ll read it back.
            </li>
            <li>
              Skip granting it a project role. A role controls what it can do inside Google Cloud itself —
              what it can read in Sheets and Drive comes entirely from what gets shared with it in step 4,
              the same as sharing a file with a colleague.
            </li>
            <li>
              Open the new service account, go to its <strong>Keys</strong> tab, and choose{" "}
              <strong>Add key → Create new key → JSON</strong>. Google downloads one file and will not show
              you its private key again — lose it, and the fix is creating a new key here, not recovering
              the old one.
            </li>
          </Substeps>
          <Todo>
            The exact current label sequence for &ldquo;Add key&rdquo;, and whether the console still calls
            the format choice &ldquo;JSON&rdquo;.
          </Todo>
          <Callout title="Treat the downloaded file like a password">
            It is a private key that authenticates as the service account with no second factor and no
            expiry. Handle it the way you would any other credential — never in a chat message, a ticket, or
            a repo — and paste it straight into the connector form in step 5 rather than keeping a copy
            somewhere you&apos;ll forget about.
          </Callout>
        </Step>

        <Step n={4} title="Share each spreadsheet and folder with the service account, and collect the IDs">
          <P>
            A service account does not inherit your own account&apos;s access to anything, even a
            spreadsheet you own — every document has to be shared with it explicitly, the same as sharing it
            with a new colleague.
          </P>
          <Substeps>
            <li>
              Copy the service account&apos;s address: the <C>client_email</C> field in the JSON key from
              step 3, shaped like <C>name@project-id.iam.gserviceaccount.com</C>.
            </li>
            <li>
              Open each spreadsheet, click <strong>Share</strong>, paste that address, set its role to{" "}
              <strong>Viewer</strong>, and send — untick &ldquo;Notify people&rdquo; first, since it
              isn&apos;t one.
            </li>
            <li>Do the same at the folder level for anything you&apos;d rather allow by folder than by file.</li>
          </Substeps>
          <Callout title="Share silently refusing the address?">
            That is the failure mode from the top of this page: a Workspace admin who has disabled sharing
            outside the organization blocks a <C>@…gserviceaccount.com</C> address exactly like any other
            outside account. Ask for a policy exception, or use the OAuth walkthrough collapsed at the
            bottom of this page instead.
          </Callout>
          <P>
            Then collect the ID of everything you just shared. Open it in a browser — the ID is the segment
            between <C>/d/</C> and <C>/edit</C>:
          </P>
          <CopyableCode
            value="https://docs.google.com/spreadsheets/d/1AbC...THIS_PART...XyZ/edit#gid=0"
            label="the spreadsheet URL shape"
          />
          <P>For a Drive folder, the ID is everything after the last slash:</P>
          <CopyableCode
            value="https://drive.google.com/drive/folders/0B1a...THIS_PART...9Zk"
            label="the Drive folder URL shape"
          />
          <Callout variant="boundary" title="This list is the boundary — not the credential">
            Sapanjai checks every request against these IDs before it calls Google, and an ID that is not on
            the list is refused even though the connector&apos;s credential could reach it perfectly well.
            That is the point: an agent that gets talked into asking for someone else&apos;s spreadsheet gets
            a refusal, not the file. It also means a sheet you forgot to list simply will not work, and that
            is the single most common reason a working connector &ldquo;can&apos;t see&rdquo; a document.
          </Callout>
          <Callout title="Folders do not cascade">
            Sharing or allowlisting a folder covers the files directly inside it — not files nested in its
            subfolders. If your documents live one level down, share and list those subfolders too. One
            folder silently granting an entire tree is exactly the kind of unbounded scope this is meant to
            prevent.
          </Callout>
        </Step>

        <Step n={5} title="Paste it all into the connector">
          <P>
            Go to <Link href="/connectors" className="text-foreground underline underline-offset-4">connectors</Link>,
            create one with the type <C>google_sheets</C>, and paste the whole JSON key from step 3 into the{" "}
            <strong>Service account key (JSON)</strong> field — it parses in the browser and echoes back the{" "}
            <C>client_email</C> so you can confirm it&apos;s the one you just shared with. (Used the OAuth
            walkthrough below instead? Fill in its client ID, client secret, and refresh token there
            instead.) Add at least one spreadsheet or folder ID from step 4. A connector cannot be created
            without its configuration, so there is no half-finished state to come back to.
          </P>
          <P>
            If a sheet&apos;s real header row is not row 1 — a title banner above it, say — set a header row
            override for it on the form. Otherwise the first row of the sheet gets read as column names.
          </P>
          <Callout>
            Nothing you type here is ever shown back to you. The configuration is encrypted the moment it
            arrives and no endpoint returns it, so a later edit replaces the whole thing rather than merging
            into it — when you change one ID, re-enter the credential too.
          </Callout>
        </Step>

        <Step n={6} title="Run a health check">
          <P>
            A new connector starts <strong>inactive</strong>. On the connectors list, use{" "}
            <strong>Run health check</strong> on its row: Sapanjai uses the credential to get a live access
            token and reads one allowlisted document with it. If that works, the connector flips to{" "}
            <strong>active</strong> and agents can use it.
          </P>
          <Callout title="What a pass actually proves">
            The probe reads the <em>first</em> spreadsheet on your list, or — if you listed only folders — the
            first folder. So a pass proves your credential, its scopes, and that one ID. It does not walk
            the rest of the list. If one document out of six misbehaves later, the health check will still
            say active.
          </Callout>
          <P>
            A failure is reported without Google&apos;s own error message, deliberately: those messages name
            accounts and files, and this one is on its way into a log. Work down this list instead.
          </P>
          <ul className="space-y-2 text-sm leading-relaxed text-muted-foreground">
            <li>
              <strong className="text-foreground">Both APIs enabled?</strong> Step 2. Enabling one and not the
              other is the most common miss.
            </li>
            <li>
              <strong className="text-foreground">Using a service account — shared with the exact address?</strong>{" "}
              A typo in the <C>client_email</C> pasted into Share produces the same failure as not sharing it
              at all, and Google gives no warning at share time either way.
            </li>
            <li>
              <strong className="text-foreground">Using OAuth — is the refresh token still alive?</strong> If
              the consent screen is still in Testing, it may already have expired — see the OAuth walkthrough
              below.
            </li>
            <li>
              <strong className="text-foreground">Credential pasted whole?</strong> A trailing space or a
              line break copied along with a secret is invisible and fatal.
            </li>
            <li>
              <strong className="text-foreground">ID, not URL?</strong> The allowlist wants the bare ID from
              step 4, not the whole address.
            </li>
          </ul>
        </Step>
      </div>

      <Disclosure summary="OAuth (refresh token) — only if your Workspace admin blocks external sharing">
        <P>
          Reach for this only if step 4 above is a dead end. A Workspace admin who has disabled sharing
          outside the organization blocks a service account&apos;s address exactly like any other outside
          account, with no per-file override. OAuth instead authorizes as a Google account that already has
          access, so it sidesteps the block entirely — at the cost of a refresh token you mint by hand and,
          per the warning below, may have to renew.
        </P>

        <div className="space-y-3">
          <h3 className="font-heading text-sm font-medium">Create an OAuth client</h3>
          <P>
            This is what produces the <C>client_id</C> and <C>client_secret</C>. Google will make you fill in
            a consent screen first, even though nobody outside your own account will ever see it.
          </P>
          <Substeps>
            <li>
              Configure the consent screen. Choose <strong>Internal</strong> if you have Google Workspace and
              only your own staff will authorize — it skips Google&apos;s review entirely. Otherwise choose{" "}
              <strong>External</strong>.
            </li>
            <li>
              On External, add the Google account you signed in with as a <strong>test user</strong>.
              Authorization below fails outright if you skip this.
            </li>
            <li>
              Create the OAuth client itself. Pick <strong>Web application</strong> as the type if you plan to
              use the OAuth Playground below, and <strong>Desktop app</strong> if you plan to use the manual
              route.
            </li>
            <li>Copy the client ID and client secret somewhere safe. You need both below and in the connector form.</li>
          </Substeps>
          <Todo>
            The console&apos;s current navigation path to the consent screen and the credentials page — these
            have moved more than once — plus the exact type names in the client-type dropdown.
          </Todo>
          <Callout variant="boundary" title="Leaving the consent screen in Testing will break this in a week">
            While an External app&apos;s publishing status is <strong>Testing</strong>, Google expires its
            refresh tokens after roughly seven days. The connector works, and then one morning it does not,
            with nothing on your side having changed. Publish the app once you have confirmed the setup works
            — or expect to redo the token exchange below every week. <em>Verify against Google&apos;s current
            policy before relying on the exact number of days.</em>
          </Callout>
        </div>

        <div className="space-y-3">
          <h3 className="font-heading text-sm font-medium">Get a refresh token with exactly these two scopes</h3>
          <P>
            A refresh token is the long-lived credential Sapanjai stores. Everything else is derived from it.
            It has to carry these two scopes and no others:
          </P>
          <CopyableCode value={SHEETS_SCOPE} label="the read-only Sheets scope" />
          <CopyableCode value={DRIVE_SCOPE} label="the read-only Drive scope" />
          <Callout variant="boundary" title="Read-only, both of them">
            Note the <C>.readonly</C> on the end of each. Sapanjai never writes to a sheet — there is no tool
            in the gateway that can — so a token carrying write scopes buys you nothing and costs you the
            difference in blast radius if it ever leaks. Granting extra scopes is the one mistake here that
            leaves no visible trace: everything works, it is just more exposed than it needs to be.
          </Callout>
          <P>There are two ways to get one. Both end with the same string.</P>

          <div className="space-y-3 rounded-md border p-4">
            <h4 className="text-sm font-medium">Option A — Google&apos;s OAuth Playground</h4>
            <P>Fewer moving parts, but it is a web app whose layout this page cannot show you.</P>
            <Substeps>
              <li>
                Add <C>https://developers.google.com/oauthplayground</C> as an authorized redirect URI on the
                OAuth client above. Without this, the Playground is rejected.
              </li>
              <li>
                Open the{" "}
                <ExtLink href="https://developers.google.com/oauthplayground">OAuth Playground</ExtLink>, and
                in its settings turn on{" "}
                <strong>Use your own OAuth credentials</strong>, then paste in your client ID and secret.
              </li>
              <li>Enter the two scopes above and authorize as the account that owns the sheets.</li>
              <li>Exchange the authorization code, and copy the refresh token out of the response.</li>
            </Substeps>
            <Todo>
              Where the settings control lives, and what the refresh token field is labelled in the response
              panel.
            </Todo>
          </div>

          <div className="space-y-3 rounded-md border p-4">
            <h4 className="text-sm font-medium">Option B — by hand</h4>
            <P>
              More typing, but every step is exact and nothing depends on a screen. Replace{" "}
              <C>YOUR_CLIENT_ID</C> and <C>YOUR_CLIENT_SECRET</C> with the values from above.
            </P>
            <Substeps>
              <li>
                Add <C>http://localhost:8080</C> as an authorized redirect URI on the OAuth client.
              </li>
              <li>
                Open this URL in a browser and approve the access. The scopes are already encoded into it:
              </li>
            </Substeps>
            <CopyableCode value={AUTH_URL} label="the authorization URL" />
            <Substeps start={3}>
              <li>
                The browser lands on a <C>localhost:8080</C> page that fails to load — that is expected,
                nothing is listening there. What you need is in the address bar: the value of the{" "}
                <C>code=</C> parameter.
              </li>
              <li>Exchange that code for tokens:</li>
            </Substeps>
            <CopyableCode value={TOKEN_EXCHANGE} label="the token exchange command" />
            <P>
              The response contains <C>refresh_token</C>. That is the value you need. It starts with{" "}
              <C>1//</C>.
            </P>
          </div>

          <Callout title="No refresh_token in the response?">
            Almost always one of two causes. Either <C>access_type=offline</C> was missing, or the account had
            already granted consent once before — in which case Google returns an access token and nothing
            else unless <C>prompt=consent</C> forces it to re-issue. Both are already in the URL above; if you
            built your own, check for them there first.
          </Callout>
        </div>

        <P>
          You still need the spreadsheet and Drive folder IDs — step 4 above shows how to find them. Skip the
          sharing part: the account you just authorized already has access, since it is the one that owns or
          can already open these documents.
        </P>
      </Disclosure>

      <div className="space-y-3 rounded-lg border bg-card p-5">
        <h2 className="font-heading text-base font-medium">Next: give an agent a key</h2>
        <P>
          The connector is the upstream. An agent still needs its own credential to reach it — mint one on{" "}
          <Link href="/mcp-keys" className="text-foreground underline underline-offset-4">
            MCP keys
          </Link>
          . Keys are scoped to this organization and can be revoked at any time without touching the connector
          you just set up.
        </P>
      </div>
    </div>
  );
}
