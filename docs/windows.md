# chcli on Windows

chcli ships as a single `chcli.exe` for Windows on x86-64 and ARM64. Everything
in the other documents applies; this page covers what is specific to Windows.

## Install

1. Download `chcli_windows_amd64.zip` (or `chcli_windows_arm64.zip` for ARM64
   machines) from the [releases page](https://github.com/nenych/chcli/releases).
   `checksums.txt` on the same page has its SHA-256:
   `Get-FileHash chcli_windows_amd64.zip` in PowerShell shows yours.
2. Unzip it and move `chcli.exe` to a folder on your `PATH`, for example
   `%USERPROFILE%\.local\bin`. To add that folder to `PATH` for your user:

   ```powershell
   [Environment]::SetEnvironmentVariable("Path", $env:Path + ";$env:USERPROFILE\.local\bin", "User")
   ```

   then open a new terminal.
3. Check: `chcli version`.

With Go installed, `go install github.com/nenych/chcli/cmd/chcli@latest`
builds and installs it into `%USERPROFILE%\go\bin`.

## Terminal

Use **Windows Terminal**, **PowerShell** or `cmd.exe` for the interactive
shell. Git Bash (mintty) and similar terminals do not present themselves as a
console to Windows programs; chcli then sees no terminal, treats the input as
a script and waits for statements on standard input. Scripted use
(`chcli -q "..."`, `--file`, pipes) works in any terminal.

The interactive shell on Windows is built on the same components as on macOS
and Linux, but has had less testing there. Please report anything odd with
`--debug` output attached (credentials are redacted).

## Shell completion

PowerShell, for the current session:

```powershell
chcli completion powershell | Out-String | Invoke-Expression
```

To load it in every session, add that line to your profile (`notepad $PROFILE`).
The release archive also contains `completions\chcli.ps1`, which you can
dot-source from the profile instead.

## Where things are stored

| What | Location |
|---|---|
| Configuration file | `%AppData%\chcli\config.yaml` (override with `--config` or `CHCLI_CONFIG`) |
| History | `%LocalAppData%\chcli\history\` |
| OAuth sessions | Windows Credential Manager (Control Panel → Credential Manager → Windows Credentials, entries named `chcli`) |
| Fallback token files | `%LocalAppData%\chcli\tokens\`, only when the Credential Manager is unavailable |

Credential Manager entries are limited in size; large token sets are stored as
several entries, which chcli manages for you.

## Pager

The optional pager (`output.pager` in the configuration file) runs a command
and pipes the result through it. `less` is not installed by default on
Windows; leave the setting empty, or install `less` (for example with
`winget install jftuga.less`) and set `pager: less -FRSX`.

## Browser login

`chcli auth login` opens your default browser and listens on a loopback port
for the redirect, exactly as on other platforms. Windows Firewall does not
prompt for loopback listeners. If a corporate policy blocks browser launches,
use `--oauth-flow device` and enter the code on any device.
