package privatefs

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateACLInheritance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private state")
	if err := MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "credentials")
	if err := os.WriteFile(child, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, child} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatalf("unexpected DACL on %s: %v %v", path, acl, err)
		}
		seenUser, seenSystem := false, false
		system, err := windows.StringToSid("S-1-5-18")
		if err != nil {
			t.Fatal(err)
		}
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
				t.Fatal("unexpected ACE type")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			switch {
			case sid.Equals(user.User.Sid):
				seenUser = true
			case sid.Equals(system):
				seenSystem = true
			default:
				t.Fatalf("unexpected trustee %s", sid.String())
			}
		}
		if !seenUser || !seenSystem {
			t.Fatal("missing user or SYSTEM access")
		}
	}
}
