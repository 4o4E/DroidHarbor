#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 BACKUP_DESTINATION PLATFORM_DATA CONFIG_DIR ADB_AUTH_DIR DROIDHARBOR_BINARY" >&2
  exit 2
fi
for path in "$@"; do
  [[ "$path" == /* ]] || { echo "all backup paths must be absolute" >&2; exit 2; }
done
backup=$(realpath -m -- "$1")
platform_data=$(realpath -e -- "$2")
config_dir=$(realpath -e -- "$3")
adb_auth=$(realpath -e -- "$4")
binary=$(realpath -e -- "$5")
for path in "$platform_data" "$config_dir" "$adb_auth"; do
  [[ -d "$path" && "$path" != "/" ]] || { echo "backup source directory is unavailable or unsafe" >&2; exit 1; }
done
[[ -x "$binary" ]] || { echo "DroidHarbor binary is not executable" >&2; exit 1; }
[[ ! -e "$backup" ]] || { echo "backup destination already exists; refusing overwrite" >&2; exit 1; }
for source in "$platform_data" "$config_dir" "$adb_auth"; do
  case "$backup/" in "$source/"*) echo "backup destination must not be inside a source directory" >&2; exit 1 ;; esac
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
compose=(docker compose)
if [[ -n "${DH_COMPOSE_ENV_FILE:-}" ]]; then
  [[ "$DH_COMPOSE_ENV_FILE" == /* && -f "$DH_COMPOSE_ENV_FILE" ]] || { echo "DH_COMPOSE_ENV_FILE must be an existing absolute file" >&2; exit 1; }
  compose+=(--env-file "$DH_COMPOSE_ENV_FILE")
fi
compose+=(--project-directory "$script_dir" -f "$script_dir/compose.yaml")
running_text=$("${compose[@]}" ps --status running --services)
running=()
if [[ -n "$running_text" ]]; then
  mapfile -t running <<<"$running_text"
fi
resume() {
  if ((${#running[@]})); then
    "${compose[@]}" start "${running[@]}" >/dev/null
  fi
}
trap resume EXIT
"${compose[@]}" stop --timeout 45 platform adb >/dev/null
remaining=$("${compose[@]}" ps --status running --services)
if [[ -n "$remaining" ]]; then
  echo "containers did not stop cleanly; backup aborted" >&2
  exit 1
fi
mkdir -m 700 -- "$backup"
mkdir -m 700 -- "$backup/platform-data" "$backup/config" "$backup/adb-auth"
cp -a -- "$platform_data/." "$backup/platform-data/"
cp -a -- "$config_dir/." "$backup/config/"
cp -a -- "$adb_auth/." "$backup/adb-auth/"
"$binary" version >"$backup/platform-version.json"
[[ -s "$backup/platform-version.json" ]] || { echo "DroidHarbor version command returned no metadata" >&2; exit 1; }
printf '%s\n' 'format=droidharbor-backup-v1' >"$backup/manifest.txt"
echo "Stopped-state backup created at $backup. Platform data, configuration/TLS, and ADB authorization are separate."