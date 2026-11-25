package pgtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

// A release runs each test binary holding its lock on an inherited
// descriptor, so anything pgtest leaves running keeps the release locked.
func TestStoppedClusterReleasesInheritedDescriptors(t *testing.T) {
	if os.Getenv("FMN_PGTEST_CHILD") != "" {
		Database(t)
		return
	}
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = []string{"FMN_PGTEST_CHILD=1"}
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "FMN_TEST_DATABASE_URL=") {
			child.Env = append(child.Env, variable)
		}
	}
	child.ExtraFiles = []*os.File{lock}
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("test binary using a disposable cluster: %v\n%s", err, out)
	}
	lock.Close()
	again, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if err := syscall.Flock(int(again.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock still held after the test binary exited: %v", err)
	}
}
