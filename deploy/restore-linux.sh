#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 BACKUP_SOURCE PLATFORM_DATA_TARGET CONFIG_DIR_TARGET ADB_AUTH_DIR_TARGET DROIDHARBOR_BINARY" >&2
  exit 2
fi
for path in "$@"; do
  [[ "$path" == /* ]] || { echo "all restore paths must be absolute" >&2; exit 2; }
done
backup=$(realpath -e -- "$1")
platform_target=$(realpath -m -- "$2")
config_target=$(realpath -m -- "$3")
adb_target=$(realpath -m -- "$4")
binary=$(realpath -e -- "$5")
[[ -f "$backup/manifest.txt" && "$(cat -- "$backup/manifest.txt")" == 'format=droidharbor-backup-v1' && -s "$backup/platform-version.json" ]] || { echo "backup manifest/version is missing or unsupported" >&2; exit 1; }
for component in platform-data config adb-auth; do
  [[ -d "$backup/$component" ]] || { echo "backup component is missing" >&2; exit 1; }
done
[[ -x "$binary" ]] || { echo "DroidHarbor binary is not executable" >&2; exit 1; }
for path in "$platform_target" "$config_target" "$adb_target"; do
  [[ ! -e "$path" ]] || { echo "restore target already exists; refusing overwrite" >&2; exit 1; }
done
for target in "$platform_target" "$config_target" "$adb_target"; do
  case "$target/" in "$backup/"*) echo "restore targets must not be inside the backup source" >&2; exit 1 ;; esac
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
compose=(docker compose)
if [[ -n "${DH_COMPOSE_ENV_FILE:-}" ]]; then
  [[ "$DH_COMPOSE_ENV_FILE" == /* && -f "$DH_COMPOSE_ENV_FILE" ]] || { echo "DH_COMPOSE_ENV_FILE must be an existing absolute file" >&2; exit 1; }
  compose+=(--env-file "$DH_COMPOSE_ENV_FILE")
fi
compose+=(--project-directory "$script_dir" -f "$script_dir/compose.yaml")
"${compose[@]}" stop --timeout 45 platform adb >/dev/null
remaining=$("${compose[@]}" ps --status running --services)
if [[ -n "$remaining" ]]; then
  echo "containers did not stop cleanly; restore aborted" >&2
  exit 1
fi
mkdir -m 700 -- "$platform_target" "$config_target" "$adb_target"
cp -a -- "$backup/platform-data/." "$platform_target/"
cp -a -- "$backup/config/." "$config_target/"
cp -a -- "$backup/adb-auth/." "$adb_target/"
"$binary" migrate --database "$platform_target/droidharbor.db" >/dev/null
printf '%s\n' 'Restore copied platform state/config and separate ADB authorization, and validated the database with migrate.'
printf '%s\n' 'Services remain stopped. Point deployment configuration at these targets; next platform startup performs normal boot recovery and does not resume old leases.'