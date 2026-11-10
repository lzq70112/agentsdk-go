# Security (v2)

agentsdk-go v2 uses a YOLO-default execution model with two enforcement layers:

1. **Sandbox** (`pkg/sandbox/`): filesystem/network/resource isolation (no approval workflow).
2. **Safety hook** (`pkg/hooks/safety.go`): Go-native `PreToolUse` check that blocks catastrophic `bash` commands before user shell hooks run.

## Safety Hook

- Runs before user-configured shell hooks.
- Blocks a small, explicit blocklist of destructive patterns (e.g. `rm -rf /`, `dd`, `mkfs`, `fdisk`, `shutdown`, `reboot`, `sudo`).
- Disable with `api.Options{DisableSafetyHook: true}`.

## Sandbox

Sandbox is about isolation, not permission prompts:

- Filesystem roots and path traversal controls
- Network restrictions (when enabled/configured)
- Resource limits

The sandbox manager is owned by tool execution (`pkg/tool/`) and is configured via `.agents/settings.json` and/or `api.Options`.

## Settings Notes

`.agents/settings.json` still accepts a `permissions` object for compatibility. v2 core does not implement an approval/ask workflow; `permissions.allow` / `permissions.ask` are parsed but not enforced.

- `permissions.deny` **is enforced** at tool execution (before hooks). Supported rules:
  - `Bash` — deny the bash tool entirely.
  - `Bash(ssh:*)` — prefix match on the command (segments of compound commands are matched individually; `VAR=value cmd` env prefixes and `xargs cmd` forms are recognized).
  - `Bash(docker ps)` — exact command match (whitespace-normalized).
  - `Bash(docker * rm)` — `*` acts as a wildcard within the command.
  - Selectors on other tools (e.g. `Read(~/.ssh/**)`) are parsed but not enforced; a warning is logged at startup.
  - Static analysis limitation: commands that obfuscate the binary name (e.g. `bash -c "ssh ..."`, `$(echo ssh)`) are not detected.
- Use `permissions.additionalDirectories` to widen filesystem roots.
- Use `disallowedTools` to disable built-in tools by name.

Example:

```json
{
  "permissions": {
    "additionalDirectories": ["/data"]
  },
  "disallowedTools": ["bash"],
  "sandbox": {
    "enabled": true
  }
}
```
