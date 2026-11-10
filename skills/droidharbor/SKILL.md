---
name: droidharbor
description: Use DroidHarbor to claim a physical Android device, connect through its task-scoped native ADB gateway, operate it, renew the lease explicitly, and release it. Activate whenever a task needs a DroidHarbor-managed phone.
---

# DroidHarbor product Skill

Use this Skill only with the released `droidharbor-cli` and a configured DroidHarbor server. It describes product use, not development. The platform serves HTTP; use it on a trusted private/isolated network because HTTP does not encrypt credentials or device data. If infrastructure provides an HTTPS entry, the CLI also accepts it using the operating system's normal trust settings; there are no CLI certificate options. The shared calling machine is trusted: the local native ADB Server does not authenticate its clients, and its loopback port is not a task-isolation boundary. Mutually untrusted tasks require separate caller network environments, such as separate machines or network namespaces. Different OS users alone do not isolate loopback access.

## Prerequisites and secret handling

- Use the released Windows or Linux CLI and the locked native Android Debug Bridge 1.0.41 / platform-tools 37.0.1; `connect` rejects other versions and does not download or upgrade adb. Use the administrator-provided HTTP(S) server address. Never put credentials in URLs or follow a redirect to another server.
- Store the caller credential JSON (containing `token`) in a private file. Never print it, paste it into a command, put it in an environment variable, or commit it. Linux: protect the file as mode 0600. Windows: use a directory with an ACL granting access only to the task's run account. Keep task state in a separate private directory; Linux directory mode 0700, Windows private ACL, with its parent directory already present. The CLI manages task ADB keys automatically. Even though it supplies task-specific `HOME`, `ANDROID_USER_HOME`, and `ADB_VENDOR_KEYS`, official Windows adb still reads the current Windows profile's default user key and may generate it if missing; those variables do not isolate that profile or guarantee the default key is not read. Do not overwrite/delete existing default keys. Only the registered task key authenticates to the platform gateway.
- Use one task directory and unique task ID per task. Keep the caller credential outside that task directory where practical. Do not expose task state, task secrets, private keys, or local connection control material in logs or output.

## Check and claim

Set these values in the harness's live environment/configuration (not as secrets in command arguments): `DROIDHARBOR_SERVER`, `DROIDHARBOR_CREDENTIAL_FILE`, `DROIDHARBOR_TASK_DIR`, and `DROIDHARBOR_TASK_ID`. Substitute their values in command examples below; these names are documentation placeholders, not CLI environment-variable interfaces. Ensure the parent directory of `DROIDHARBOR_TASK_DIR` already exists and is private before claiming.

```text
droidharbor-cli version
droidharbor-cli capabilities --server <server-address> --credential-file <caller-JSON>
droidharbor-cli claim --server <server-address> --credential-file <caller-JSON> --task-dir <private-task-dir> --task-id <task-id> --tag <required-tag> [--tag <another-required-tag>]
droidharbor-cli status --task-dir <private-task-dir>
```

Confirm API major 1 and `remoteAdb` capability before connecting. Each supplied `--tag` is required; selection is not an any-tag match. A claim gets a 900-second lease. Save the task directory and original claim inputs. If claim returns result-unknown (exit 6), preserve the directory, caller credential, task ID, tags, and request ID. `status` cannot query the lease until a lease ID is saved; if it reports that the initial claim result is unknown, explicitly rerun the identical `claim` with the same directory, server, credential, task ID, and tags. The CLI reuses the saved task secret and idempotency key and the server returns the original lease/terminal result; never change the parameters, create another task directory, or use a new key.

## Connect and operate

Run `connect` in the foreground of an ordinary OS terminal and leave that process alive for all device work. It emits one readiness JSON line only once the native ADB target is online, then stays running. Use a second terminal for `env`, `renew`, `disconnect`, and `release`; no agent notification, plugin, lifecycle hook, or helper script is required. A harness may use its normal background-process facility only if it keeps this same foreground process alive and captures its stdout JSON line.

```text
droidharbor-cli connect --task-dir <private-task-dir> --adb-bin <absolute-native-adb-path>
droidharbor-cli env --task-dir <private-task-dir>
```
Windows PowerShell:

```powershell
& 'C:\tools\droidharbor-cli.exe' connect --task-dir 'C:\private\droidharbor\task-42' --adb-bin 'C:\Android\platform-tools\adb.exe'
```

Linux shell:

```sh
/opt/droidharbor/droidharbor-cli connect --task-dir /var/lib/droidharbor/task-42 --adb-bin /opt/android/platform-tools/adb
```

These commands run in the foreground until explicitly disconnected or interrupted; leave the terminal open. Use another terminal for task commands.

For each native ADB operation, use the `adbBinary` and `adbArgs` from the current readiness/env JSON exactly, followed by the native command and arguments. These JSON values are placeholders in command examples; pass each `adbArgs` entry as its own argument, not as a combined string. Retain `-H 127.0.0.1`, `-P <adbServerPort>`, and `-s 127.0.0.1:<device-endpoint-port>` explicitly. The CLI starts only its task-specific ADB Server with USB/emulator/mDNS discovery disabled; never use the calling machine's default ADB Server or default target, run global `adb kill-server`, omit `-H`, or reuse the endpoint after disconnect. Native stdout, stderr, and exit status are the operation result; do not interpret local ADB service setup as proof the remote service is supported.

A newly claimed phone may retain its previous application, screen, login, and background-process state. Observe before acting; for example, read the current screen and device state, then decide whether to navigate or continue:

```text
<adbBinary> <adbArgs...> get-state
<adbBinary> <adbArgs...> shell dumpsys window
```

Do not assume apps were closed, accounts logged out, or UI reset. Platform lease cleanup revokes task access; it does not reset application state or guarantee that a program already started on the phone stops running. Consider possible continuing side effects.

### Observe, launch and navigate an app

Use the app package requested by the task. Resolve its current launcher instead of assuming a launcher icon position or a version-specific Activity:

```text
<adbBinary> <adbArgs...> shell cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.LAUNCHER -p <package>
<adbBinary> <adbArgs...> shell am start -W -n <resolved-package/activity>
```

Common package identities: Taobao `com.taobao.taobao`, Pinduoduo `com.xunmeng.pinduoduo`, Xianyu `com.taobao.idlefish`. A successful `am start` means Android accepted the launch, not that the intended app page or search results are ready. Read the foreground window and the actual page before the next action.

If the phone is asleep, explicitly wake it when the task permits, then observe the restored interface before clicking:

```text
<adbBinary> <adbArgs...> shell input keyevent KEYCODE_WAKEUP
<adbBinary> <adbArgs...> shell dumpsys power
<adbBinary> <adbArgs...> shell dumpsys window
```

An immediate click can arrive before wake-up finishes. A black screenshot may represent a sleeping display; check current power/window state rather than treating a valid PNG as proof of a usable app screen. A credential lock requires the user's unlock action.

Android's built-in `uiautomator dump` is usable through native shell and is distinct from the unsupported uiautomator2 framework. Choose a unique task-owned XML path:

```text
<adbBinary> <adbArgs...> shell uiautomator dump --compressed /data/local/tmp/droidharbor-<task-owned-random>-ui.xml
<adbBinary> <adbArgs...> shell -T cat /data/local/tmp/droidharbor-<task-owned-random>-ui.xml
<adbBinary> <adbArgs...> shell rm -f /data/local/tmp/droidharbor-<task-owned-random>-ui.xml
```

Verify that the XML exists and parses; an idle-state failure can occur without a useful nonzero `uiautomator` exit status. Animated pages can exceed the idle wait. Use a fresh screenshot to observe such a page rather than repeating an input or changing global animation settings. Keep hierarchy/screenshot data private, extract only task-relevant nodes, and remove only this task's XML files.

Select controls from current text/content descriptions, class and bounds. Resource IDs are not always unique or present: Pinduoduo can reuse one ID across controls, and Xianyu's custom-rendered pages may expose descriptions without IDs. Use coordinates derived from the current observation for one action, then observe again. Dismiss a promotion only through its verified close control; distinguish it from coupon, purchase and chat actions.

For example, native ASCII input and a tap are:

```text
<adbBinary> <adbArgs...> shell input text usb
<adbBinary> <adbArgs...> shell input tap <current-x> <current-y>
```

Before submitting a search, read back the actual input field. The active IME can transform multiword input, and `input text` is not a general Unicode/Chinese input channel. Use ASCII only when it satisfies the requested query; required Chinese input needs an explicitly approved input method/channel. Preserve the user's keyboard settings and account state.

Inspect the submitted query, visible product results and opened product details separately. App-specific deep links need the same page-level verification; successful intent resolution/launch alone does not prove the query was applied. If search or browsing reaches a login/authorization gate, request the user's login action and report that path as blocked. Device tags do not prove that an app login is still valid. Browsing authorization alone does not authorize orders, payments, messages, account switching or coupon redemption.

### Keep the observation loop efficient

- Resolve the launcher once per app/task and reuse it until the installed app changes or launch fails. Do not repeat package discovery before each navigation.
- Start with a screenshot when page identity or visual state is the question. Request a UI hierarchy only when text, descriptions or bounds are needed; parse task-relevant nodes rather than printing the entire tree.
- Reuse a verified locator while the same page, orientation and layout remain valid. Re-observe after navigation, overlays, scrolling or rotation; do not reuse stale coordinates across those changes.
- Prefer one purposeful action followed by the smallest observation that proves its result. A search needs the actual input field checked before submit, then results checked; it does not need repeated full `dumpsys` output between each tap.
- On a dynamic page where a hierarchy dump fails its idle wait, inspect a fresh screenshot instead of polling `uiautomator dump`. Do not replay the previous action or change animation settings to obtain a tree.
- Query power/window state when a black image, wake-up or unexpected foreground page requires diagnosis, not on every step. Avoid fixed sleeps and repeated identical screenshots when the required state is already observable.


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
droidharbor-cli renew --task-dir <private-task-dir>
```

Do not add automatic retries, reconnect, replay a device action, claim a new task, or switch devices. If a shell/input/file/install command is interrupted after it may have reached Android, treat the result as unknown and inspect current state before deciding on any new action; never replay automatically or repeat the action manually without reconciling its outcome. A result-unknown initial claim is different: use only the exact same claim identity and parameters described above to retrieve its idempotent original result.

For a fresh `connect` attempt that has not submitted creation, or receives a confirmed 4xx creation rejection, the unused local staging record and ADB home are removed. A subsequent `env`/`disconnect` can correctly return `CONNECTION_STATE_MISSING`; this is not an unknown successful creation. A genuinely lost response or an earlier unconfirmed attempt keeps its original key and idempotency identity. Do not discard that state or report it closed merely because a later operation fails.

## Finish and failures

After device work, stop the local connection explicitly, then release the lease:

```text
droidharbor-cli disconnect --task-dir <private-task-dir>
droidharbor-cli release --task-dir <private-task-dir>
```

Disconnect closes this task's recorded connection; release ends its lease authorization. CLI cleanup is limited to resources recorded for this task and process identities it can verify; never kill an unowned PID/process, target a guessed port, or use a global ADB command. Do not assume lease release resets phone applications. If the harness crashes, there is no hidden renewal; authorization expires under the lease rules.

CLI exit codes are stable: `0` success; `1` output failure; `2` argument/local configuration error; `3` identity/permission failure; `4` connection or service response failure; `5` capability/business-state conflict (including busy, no matching device, ended lease); `6` write result unknown. For unknown renew/release outcomes, run `status` first; do not repeat the write blindly. For unknown disconnect/connection outcomes, query `status` and `env` only as supported to determine current state; never trust a stale local port. Unknown results do not authorize replaying device-side shell/input/file/install operations, retrying another write, or choosing another device; initial claim recovery is only the identical idempotent operation described above. Use the error's request ID when escalating an unresolved service result.
