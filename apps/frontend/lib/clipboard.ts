/**
 * Copies `value` to the clipboard, reporting whether it worked rather than
 * throwing.
 *
 * The Clipboard API is absent in jsdom and in any non-secure-context browser
 * tab (plain http on a LAN address, which is how a self-hosted dashboard
 * often gets reached first), so every caller has to be able to fall back to
 * telling the user to select and copy by hand. A rejected promise would make
 * that the caller's problem at every call site; a boolean makes it one
 * branch.
 */
export async function copyToClipboard(value: string): Promise<boolean> {
  try {
    if (!navigator.clipboard?.writeText) return false;
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}
