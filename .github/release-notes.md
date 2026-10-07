## Install

- **Homebrew** (macOS, Linux): `brew install nenych/tap/chcli`
- **Script** (macOS, Linux): `curl -fsSL https://raw.githubusercontent.com/nenych/chcli/main/install.sh | sh`
- **Windows:** unzip `chcli_windows_amd64.zip` (or `arm64`) and put `chcli.exe` in a folder on your `PATH`, for example `%USERPROFILE%\.local\bin`. Use Windows Terminal or PowerShell for the interactive shell; see [docs/windows.md](https://github.com/nenych/chcli/blob/main/docs/windows.md).
- **Any platform with Go:** `go install github.com/nenych/chcli/cmd/chcli@latest`

The macOS binaries are not signed by Apple, so one downloaded here with a browser is blocked by Gatekeeper; Homebrew and the script do not have that problem. Each archive contains `LICENSE`, `THIRD_PARTY_NOTICES.md` and shell completion scripts; `checksums.txt` holds the SHA-256 of every archive.
