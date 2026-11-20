---
name: droidharbor
description: Use DroidHarbor to claim a physical Android device, connect through its task-scoped native ADB gateway, operate it, renew the lease explicitly, and release it. Activate whenever a task needs a DroidHarbor-managed phone.
---

# DroidHarbor product Skill

Use this Skill only with the released `droidharbor-cli` and a configured DroidHarbor server. It describes product use, not development. The platform serves HTTP; use it on a trusted private/isolated network because HTTP does not encrypt credentials or device data. If infrastructure provides an HTTPS entry, the CLI also accepts it using the operating system's normal trust settings; there are no CLI certificate options. The shared calling machine is trusted: the local native ADB Server does not authenticate its clients, and its loopback port is not a task-isolation boundary. Mutually untrusted tasks require separate caller network environments, such as separate machines or network namespaces. Different OS users alone do not isolate loopback access.

## Install and select the bundled CLI

The client release is `DroidHarbor-skill.zip`. Extract and install its complete `droidharbor` directory as a Skill; keep `SKILL.md` and `scripts` together. Resolve this installed Skill directory to an absolute path, then select:

- Windows amd64: `<skill-directory>\scripts\windows-amd64\droidharbor-cli.exe`.
- Linux amd64: `<skill-directory>/scripts/linux-amd64/droidharbor-cli`. After extraction, run `chmod u+x` on that binary once.

In every command below, `<cli>` means that selected absolute binary path, quoted for the shell; PowerShell requires `&` before a quoted executable path. No global PATH installation or separate CLI download is needed. Run `<cli> version` first. Supply the locked native ADB separately. Human users access the server in a browser and do not need this Skill or CLI.

## Prerequisites and secret handling

- Use the released Windows or Linux CLI and the locked native Android Debug Bridge 1.0.41 / platform-tools 37.0.1; `connect` rejects other versions and does not download or upgrade adb. Use the administrator-provided HTTP(S) server address. Never put credentials in URLs or follow a redirect to another server.
- Store the caller credential JSON (containing `token`) in a private file. Never print it, paste it into a command, put it in an environment variable, or commit it. Linux: protect the file as mode 0600. Windows: use a directory with an ACL granting access only to the task's run account. Keep task state in a separate private directory; Linux directory mode 0700, Windows private ACL, with its parent directory already present. The CLI manages task ADB keys automatically. Even though it supplies task-specific `HOME`, `ANDROID_USER_HOME`, and `ADB_VENDOR_KEYS`, official Windows adb still reads the current Windows profile's default user key and may generate it if missing; those variables do not isolate that profile or guarantee the default key is not read. Do not overwrite/delete existing default keys. Only the registered task key authenticates to the platform gateway.
- Use one task directory and unique task ID per task. Keep the caller credential outside that task directory where practical. Do not expose task state, task secrets, private keys, or local connection control material in logs or output.

## Check and claim

Set these values in the harness's live environment/configuration (not as secrets in command arguments): `DROIDHARBOR_SERVER`, `DROIDHARBOR_CREDENTIAL_FILE`, `DROIDHARBOR_TASK_DIR`, and `DROIDHARBOR_TASK_ID`. Substitute their values in command examples below; these names are documentation placeholders, not CLI environment-variable interfaces. Ensure the parent directory of `DROIDHARBOR_TASK_DIR` already exists and is private before claiming.

```text
<cli> version
<cli> capabilities --server <server-address> --credential-file <caller-JSON>
<cli> claim --server <server-address> --credential-file <caller-JSON> --task-dir <private-task-dir> --task-id <task-id> --tag <required-tag> [--tag <another-required-tag>]
<cli> status --task-dir <private-task-dir>
```

Confirm API major 1 and `remoteAdb` capability before connecting. `devices` returns enrolled device IDs, exact display names, tags, connection/allocation states, and discovery readiness without management endpoints or task identities. When the user specifies a phone, use `claim ... --device-name <exact-name>` or `claim ... --device-id <complete-device-id>` (mutually exclusive). A duplicate name returns `DEVICE_NAME_AMBIGUOUS` with candidate IDs; list devices and choose the intended complete ID. A missing, offline, occupied, disabled, or tag-mismatched selected phone fails explicitly; never silently claim another phone. Each supplied `--tag` is required, including with a device selector.

When the user names one or more device tags, pass every required `--tag` in the **first** claim. Use `devices` only when you need to disambiguate a device or diagnose a failure, not as a substitute for an atomic tagged claim. On `NO_MATCHING_DEVICE` or `DEVICE_UNAVAILABLE`, report the result and keep the selection criteria. Do not claim without the tags or start looking for an APK on another device.

A claim gets a 900-second lease. Save the task directory and original claim inputs. If claim returns result-unknown (exit 6), preserve the directory, caller credential, task ID, tags, device selector, and request ID. `status` cannot query the lease until a lease ID is saved; if it reports that the initial claim result is unknown, explicitly rerun the identical `claim` with the same directory, server, credential, task ID, tags, and device selector. The CLI reuses the saved task secret and idempotency key. If the device was permanently deleted, the replay returns `LEASE_ENDED` instead of a lease record; it cannot claim a different device. Never change the parameters, create another task directory, or use a new key to recover an unknown result.

## Connect and operate

Keep the foreground `connect` process alive for all device work. Wait for its one stdout readiness JSON line (`status: "active"`), emitted after ADB reports `device`, the platform confirms the same connection is active, and the task directory lock is released. Use the harness's standard output-wait facility or a second terminal; do not use a fixed sleep or wait for process exit as a readiness signal. If the process fails or no readiness arrives by the caller's deadline, inspect its error; do not proceed on a guessed port. No plugin, lifecycle hook, or helper script is required.

```text
<cli> connect --task-dir <private-task-dir> --adb-bin <absolute-native-adb-path>
<cli> env --task-dir <private-task-dir>
```
Windows PowerShell:

```powershell
& 'C:\skills\droidharbor\scripts\windows-amd64\droidharbor-cli.exe' connect --task-dir 'C:\private\droidharbor\task-42' --adb-bin 'C:\Android\platform-tools\adb.exe'
```

Linux shell:

```sh
/opt/skills/droidharbor/scripts/linux-amd64/droidharbor-cli connect --task-dir /var/lib/droidharbor/task-42 --adb-bin /opt/android/platform-tools/adb
```

These commands run in the foreground until explicitly disconnected or interrupted; leave the terminal open. Use another terminal for task commands.

For each native ADB operation, use the `adbBinary` and `adbArgs` from the current readiness/env JSON exactly, followed by the native command and arguments. These JSON values are placeholders in command examples; pass each `adbArgs` entry as its own argument, not as a combined string. Retain `-H 127.0.0.1`, `-P <adbServerPort>`, and `-s 127.0.0.1:<device-endpoint-port>` explicitly. The CLI starts only its task-specific ADB Server with USB/emulator/mDNS discovery disabled; never use the calling machine's default ADB Server or default target, run global `adb kill-server`, omit `-H`, or reuse the endpoint after disconnect. Native stdout, stderr, and exit status are the operation result; do not interpret local ADB service setup as proof the remote service is supported.

A newly claimed phone may retain its previous application, screen, login, and background-process state. Observe before acting; for example, read the current screen and device state, then decide whether to navigate or continue:

```text
<adbBinary> <adbArgs...> get-state
<adbBinary> <adbArgs...> shell dumpsys window
```

Do not assume apps were closed, accounts logged out, or UI reset. Platform lease cleanup revokes task access; it does not reset application state or guarantee that a program already started on the phone stops running. Consider possible continuing side effects.

### Launch, find and act with evidence

Use `app` through the bundled CLI on the live task connection. It resolves the current verified task ADB environment internally. Use the app's actual package: Taobao `com.taobao.taobao`, Pinduoduo `com.xunmeng.pinduoduo`, Xianyu `com.taobao.idlefish`. Use app-specific search/navigation rules from the relevant business Skill, not from this device tool.

| Command | What it proves |
| --- | --- |
| `app open --package <package>` | Resolves the installed launcher, starts it once, and returns the actual foreground and a first-screen PNG. Missing launcher is a stop, not a reason to guess an Activity or install an APK. `foregroundVerified` means the app got focus, not that its business page loaded. |
| `app find --package <package> --target <selector>` | Checks a single current observation. Returns the matching controls only. Multiple candidates mean no unique locator; refine by class, text, description, ID, or state. |
| `app tap --package <package> --target <selector> <expectation>` | Confirms one current clickable target, taps once, then waits for the given outcome. An already-satisfied condition does not prove a new action. |
| `app input --package <package> --target <selector> --text <value>` | Confirms the focused editable control. To replace text, it first confirms the old value was fully selected, sends the edit once, then reads back the exact value from the same editor. It never submits. If the field already contains the verified value, it sends no keys. |
| `app wait --package <package> <expectation>` | Observes only. Useful after an explicitly chosen native action or to verify a page after launch. With no additional condition it waits for the package to get focus. |

Each command requires `--task-dir <private-task-dir>`. `--timeout` is 12s by default (2s–60s); this is a deadline, not a delay. A selector is one JSON object; keys `id`, `class`, `text`, `description`, `enabled`, `focused`, `clickable`, and `selected` match exactly and are ANDed. Form the expectation with `--expect-activity '<package>/Activity'`, `--expect-package <destination-package>`, and/or `--expect-node <selector>`; every repeated `--expect-node` must be present in the same current observation. Use `--expect-node` on `open` to also verify a specific page (`pageVerified`). For in-app navigation, use a destination title/control rather than the app's unchanged package. Include a query/model marker when the previous result page can display the same title.

Examples show shell quoting for POSIX shells and modern PowerShell native argument passing; an argument-vector tool passes the JSON as one argument without the surrounding single quotes:

```text
<cli> app open --task-dir <task-dir> --package com.example.catalog
<cli> app find --task-dir <task-dir> --package com.example.catalog --target '{"id":"com.example.catalog:id/query"}'
<cli> app input --task-dir <task-dir> --package com.example.catalog --target '{"id":"com.example.catalog:id/query"}' --text RTX5090
<cli> app tap --task-dir <task-dir> --package com.example.catalog --target '{"text":"Search","clickable":true}' --expect-node '{"text":"Results"}' --expect-node '{"description":"Results for RTX5090"}'
```

Windows PowerShell 5.1 native command invocation requires backslash-escaped inner quotes, e.g. `--target '{\"text\":\"Search\"}'`. Keep actual locators grounded in the current app; the example package is illustrative.

Read JSON on stdout on success and `details` in stderr JSON on failure. `dispatch: "not_sent"` means no mutating action was issued; `"sent"` with `verified: false` means the command was issued but the result was not proven; `"delivery_unknown"` means the command may already have reached Android. A timeout includes the last observed foreground/matches and their timestamp where available. Waits only observe; use the last evidence to reconcile the outcome before deciding on any next action. `APP_TARGET_AMBIGUOUS` includes the match count and a bounded candidate list, and does not choose a target.

If controls are not exposed or the UI tree cannot be read reliably, use the returned current `screenshot.path` as visual evidence; the tool has **not** automatically verified the condition. An access error may instead leave `screenshotError`. Keep PNGs within the private task directory and remove the task's copies when finished. Do not replace a screenshot with a stale one or repeatedly pull the XML. If a visually grounded step needs native input, use the live `adbBinary` and `adbArgs` above, issue it once, and verify against the resulting screen or `app wait`. Do not treat a visual guess as a matched control or proven field value.

If the phone is asleep, an authorized explicit native wake-up (`shell input keyevent KEYCODE_WAKEUP`) must be followed by observation before clicking. A black PNG is not proof of a usable display; diagnose with `dumpsys power/window` when needed. A credential lock or app login gate requires the user's action. A device tag does not prove an installed app or valid login. Browsing authorization does not authorize orders, payments, messages, account switching, or coupon redemption.

`app input` uses Android native input for printable ASCII (not literal `%s`). Submit only after `verified: true` and the actual `fieldValue` exactly equals the requested query. Raw node text can be a placeholder, not proof. Do not install or change an IME to work around a rejected input; required Chinese or other Unicode needs an explicitly approved channel. The built-in `uiautomator dump` used for evidence is not the unsupported uiautomator2 framework.


### Binary-safe screenshot and files

Capture PNG bytes directly, never through text-oriented decoding or redirection. On Windows use `cmd.exe` native binary redirection, not legacy PowerShell `>`:

```bat
<adbBinary> <adbArgs...> exec-out screencap -p > screenshot.png
```

Linux shell:

```sh
<adbBinary> <adbArgs...> exec-out screencap -p > screenshot.png
```

Push and pull use native sync and preserve arbitrary binary data; use an ordinary local binary file:

```text
<adbBinary> <adbArgs...> push <local-binary-file> /sdcard/Download/input.bin
<adbBinary> <adbArgs...> pull /sdcard/Download/output.bin <local-output-file>
```

Check the command's native exit status and the resulting file. Do not pipe PNG through a text API or print its contents. Do not assume unsupported services, including forward/reverse, are available. Appium and uiautomator2 are reserved but currently unsupported and incompatible; do not treat them as available.

`exec-out` is a raw output stream, not a remote exit-status protocol. A broken connection can still produce local exit code 0; do not infer successful completion from that alone. Validate the expected binary output, and treat interrupted device work as outcome-unknown. Use supported `shell_v2` commands when separate stdout/stderr and the remote exit status are required.

## Renew during long work

Renewal is explicit and independent of the foreground connection. A lease lasts 900 seconds from each successful claim/renewal; schedule renewals with enough margin before expiry, including while a long native command is running. The connect process does not renew, and traffic does not renew. Use the normal harness facility to issue a second command while the foreground process remains alive:

```text
<cli> renew --task-dir <private-task-dir>
```

Do not add automatic retries, reconnect, replay a device action, claim a new task, or switch devices. If a shell/input/file/install command is interrupted after it may have reached Android, treat the result as unknown and inspect current state before deciding on any new action; never replay automatically or repeat the action manually without reconciling its outcome. A result-unknown initial claim is different: use only the exact same claim identity and parameters described above to retrieve its idempotent original result.

For a fresh `connect` attempt that has not submitted creation, or receives a confirmed 4xx creation rejection, the unused local staging record and ADB home are removed. A subsequent `env`/`disconnect` can correctly return `CONNECTION_STATE_MISSING`; this is not an unknown successful creation. A genuinely lost response or an earlier unconfirmed attempt keeps its original key and idempotency identity. Do not discard that state or report it closed merely because a later operation fails.

## Finish and failures

After device work, stop the local connection explicitly, then release the lease:

```text
<cli> disconnect --task-dir <private-task-dir>
<cli> release --task-dir <private-task-dir>
```

Disconnect closes this task's recorded connection; release ends its lease authorization. CLI cleanup is limited to resources recorded for this task and process identities it can verify; never kill an unowned PID/process, target a guessed port, or use a global ADB command. Do not assume lease release resets phone applications. If the harness crashes, there is no hidden renewal; authorization expires under the lease rules.

CLI exit codes are stable: `0` success; `1` output failure; `2` argument/local configuration error; `3` identity/permission failure; `4` connection or service response failure; `5` capability/business-state conflict (including busy, no matching device, ended lease); `6` write result unknown. For unknown renew/release outcomes, run `status` first; do not repeat the write blindly. For unknown disconnect/connection outcomes, query `status` and `env` only as supported to determine current state; never trust a stale local port. Unknown results do not authorize replaying device-side shell/input/file/install operations, retrying another write, or choosing another device; initial claim recovery is only the identical idempotent operation described above. Use the error's request ID when escalating an unresolved service result.
