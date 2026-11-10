//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestPrivateWindowsACLWithOwnerWithoutWriteOwner(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := tokenUser.User.Sid
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if directory {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.WriteFile(path, []byte("private state"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Modify excludes WRITE_OWNER. The owner can still replace its DACL.
			descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;0x1301bf;;;" + sid.String() + ")")
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := descriptor.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
				sid, nil, dacl, nil); err != nil {
				t.Fatal(err)
			}
			if err = setPrivateWindowsACL(path, directory); err != nil {
				t.Fatalf("owner without WRITE_OWNER must be able to protect task state: %v", err)
			}
			protected, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			owner, _, err := protected.Owner()
			if err != nil || !owner.Equals(sid) {
				t.Fatalf("task state owner changed: %v", err)
			}
			dacl, _, err = protected.DACL()
			if err != nil || dacl.AceCount != 1 {
				t.Fatalf("task state must grant access only to its owner: %v", err)
			}
			var ace *windows.ACCESS_ALLOWED_ACE
			if err = windows.GetAce(dacl, 0, &ace); err != nil {
				t.Fatal(err)
			}
			trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			const fileAllAccess = windows.ACCESS_MASK(0x001F01FF)
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != fileAllAccess || !trustee.Equals(sid) {
				t.Fatal("private task state must grant full control exclusively to the current user")
			}
		})
	}
}
