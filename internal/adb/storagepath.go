package adb

import (
	"path"
	"strings"
	"unicode/utf8"
)

// SharedStorage is the public Android path, not the implementation-specific
// path obtained after resolving the current user's emulated storage.
const SharedStorage = "/sdcard"

const sharedTrashName = ".droidharbor-trash"

// SharedTrash is managed by the recycle-bin API, not ordinary uploads.
const SharedTrash = SharedStorage + "/" + sharedTrashName

func ValidDevicePath(value string) bool {
	return len(value) <= 1024 && utf8.ValidString(value) && strings.HasPrefix(value, "/") &&
		!strings.ContainsAny(value, "\x00\\") && path.Clean(value) == value
}

func IsSharedPath(value string) bool {
	return value == SharedStorage || strings.HasPrefix(value, SharedStorage+"/")
}

func IsSharedTrashPath(value string) bool {
	return value == SharedTrash || strings.HasPrefix(value, SharedTrash+"/")
}

// Resolve the public alias on the phone, then compare physical paths. The
// caller performs I/O relative to the resulting cwd, not the earlier alias.
const SharedDirectorySetup = `shared=$(cd -P /sdcard 2>/dev/null && pwd -P) || exit 66
`

const SharedDirectoryCheck = `case "$(pwd -P)/" in "$shared/"*) ;; *) exit 67;; esac
`

const SharedUserDirectoryCheck = SharedDirectoryCheck + `case "$(pwd -P)/" in "$shared/` + sharedTrashName + `/"*) exit 67;; esac
`
