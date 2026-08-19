## 2026-08-19 - Accessible Clipboard Copy Feedback
**Learning:** Copy buttons that dynamically change text (e.g., from "Copy" to "Copied") provide visual feedback to sighted users, but screen readers do not announce these visual text changes unless accompanied by an `aria-live` region.
**Action:** When adding or updating copy-to-clipboard buttons, include a visually hidden (`sr-only`) span with `aria-live="polite"` that conditionally announces when the item has been copied to the clipboard.
