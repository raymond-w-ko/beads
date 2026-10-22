---
title: "bd supersede"
description: "Mark an issue as superseded by a replacement issue"
---

{/* AUTO-GENERATED: do not edit manually */}

Generated from `bd help --doc supersede`.

Mark an issue as superseded by a different (replacement) issue.

The superseded issue is automatically closed with a reference to the replacement.
Useful for design docs, specs, and evolving artifacts.

Issues blocked by the superseded issue now block on the replacement: each
inbound blocks edge moves to the replacement, so they wait for it instead.
Use --keep-blockers to leave those edges on the superseded issue.

Examples:
  bd supersede bd-old --with bd-new    # Mark bd-old as superseded by bd-new

```
bd supersede <id> --with <new> [flags]
```

**Flags:**

```
      --keep-blockers   Leave issues blocked by the superseded issue blocked on it instead of the replacement
      --with string     Replacement issue ID (required)
```
