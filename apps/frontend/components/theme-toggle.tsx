"use client";

import { useSyncExternalStore } from "react";
import { MonitorIcon, MoonIcon, SunIcon } from "lucide-react";
import { useTheme } from "next-themes";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

// Three mutually exclusive choices, so this is a radio group rather than a
// cycling button — "system" is the default and would be unreachable if the
// control only toggled between light and dark.
const OPTIONS = [
  { value: "light", label: "Light", Icon: SunIcon },
  { value: "dark", label: "Dark", Icon: MoonIcon },
  { value: "system", label: "System", Icon: MonitorIcon },
] as const;

// A store that never changes, so the snapshot differs only between the server
// render (false) and every client render (true) — the same useSyncExternalStore
// shape lib/org/active-org.ts uses to read client-only state without an effect.
const neverChanges = () => () => {};
const onClient = () => true;
const onServer = () => false;

export function ThemeToggle() {
  const { theme, resolvedTheme, setTheme } = useTheme();

  // The active theme comes from localStorage or the OS, both of which only
  // exist on the client. Render the icon slot empty until hydration rather
  // than guessing on the server and hydrating into a mismatch; the button
  // keeps its size either way, so nothing shifts.
  const hydrated = useSyncExternalStore(neverChanges, onClient, onServer);

  const ActiveIcon = resolvedTheme === "dark" ? MoonIcon : SunIcon;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<Button variant="ghost" size="icon-sm" aria-label="Change theme" />}
      >
        {hydrated ? <ActiveIcon className="size-4" /> : <span className="size-4" />}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-36">
        <DropdownMenuRadioGroup
          value={theme ?? "system"}
          onValueChange={(value) => setTheme(String(value))}
        >
          {OPTIONS.map(({ value, label, Icon }) => (
            <DropdownMenuRadioItem key={value} value={value}>
              <Icon className="size-4 text-muted-foreground" />
              {label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
