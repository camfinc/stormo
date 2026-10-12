package privatefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		descriptor := sd.String()
		if !strings.Contains(descriptor, ";;;SY)") || !strings.Contains(descriptor, ";;;"+user.User.Sid.String()+")") {
			t.Fatalf("unexpected trustees: %s", descriptor)
		}
	}
}
