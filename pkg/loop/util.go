package loop

import (
	"os"
	"path/filepath"
	"sort"
)

func sortStrings(s []string) { sort.Strings(s) }

// writeAtomic writes p through a temporary file in its directory, synced and renamed into place,
// so a reader never sees half of it.
func writeAtomic(p string, body []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(body)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
