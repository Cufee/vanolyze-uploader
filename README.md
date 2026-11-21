# Vanolyze Uploader

Vanolyze Uploader archives VALORANT replays to an S3-compatible bucket you configure. The Windows build runs in the notification area. The Linux build uploads replays from a local directory in headless mode.

## Install and configure

1. Download the Windows or Linux amd64 binary and its matching `.minisig` from [Releases](https://github.com/Cufee/vanolyze-uploader/releases/latest).
2. Verify the download with the maintainer's minisign public key before running it.

   ```sh
   minisign -Vm vanolyze-uploader-windows-amd64.exe -P "$PUBLIC_KEY"
   ```

3. Put the executable in a writable directory and start it. On Linux, make it executable with `chmod +x` first and pass `-demos-dir /path/to/replays`. The Windows build enables per-user startup when using the standard config location.
4. Supply your own `bucket`, `region`, `access_key_id` and `secret_access_key` in the generated JSON file. Set `endpoint` for an S3-compatible provider, and `session_token` when the credentials require one. The optional `server` field takes your site's origin and defaults to empty.

| Platform | Default config file |
| --- | --- |
| Windows | `%APPDATA%\Vanolyze\config.json` |
| Linux | `$XDG_CONFIG_HOME/Vanolyze/config.json` or `~/.config/Vanolyze/config.json` |

Use `-config PATH` to select a specific file. The uploader also looks for an existing `config.json` in the working directory, beside the executable, and in its parent directory. A relative `data_dir` is resolved beside the config. Use `-selfcheck` to validate settings offline or `-once` to scan once and exit.

## Build

Use the pinned Nix shell to check the code and build both binaries.

```sh
env -u GOROOT nix develop -c make check
env -u GOROOT nix develop -c make build
```

The static amd64 binaries are written to `dist/`. Release builds inject the version and release sequence at link time.

## Signing and updates

The release workflow builds both binaries from a stable `vMAJOR.MINOR.PATCH` tag. It signs each binary and `manifest.json` with minisign, verifies all three signatures against the key embedded in the build, and attaches the files and detached signatures to the GitHub release.

Before the first release, generate a key pair with `minisign -G`. Put the public key's second line in `internal/release/keys.go` and publish the public key through a trusted channel. In the GitHub Actions `prod` environment, set `MINISIGN_SECRET_KEY` to the protected secret key file's contents and `MINISIGN_PASSWORD` to its password. The user's first verification should use the public key obtained through that trusted channel. An installed release prints its embedded key with `-release-key`.

The uploader checks GitHub at startup and about every six hours, or when you select **Check for updates** in the Windows tray. It requires a valid manifest and binary signature, a matching size and SHA-256, and both a newer version and a higher sequence. Set `update.auto` to `false` to check without installing. Updates wait until the current scan ends. If the replacement fails its self-check or local startup, the previous executable is restored. A backup remains next to the executable after a successful update. If Windows loses power between the two renames, restore the `.old.exe` backup to the original name before starting it.
