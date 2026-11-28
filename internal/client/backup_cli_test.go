package client

import (
	"reflect"
	"strings"
	"testing"
)

// backupArgs maps a backup command line to the tool call it makes.
func backupArgs(t *testing.T, args ...string) (string, map[string]any, error) {
	t.Helper()
	pos, opt, err := parse(args)
	if err != nil {
		t.Fatal(err)
	}
	name, params, err := commandCall(pos, opt, strings.NewReader(""))
	if err == nil {
		err = addFlagParams(name, params, opt)
	}
	return name, params, err
}

func TestBackupCommandControlsADriveDestination(t *testing.T) {
	name, params, err := backupArgs(t, "backup", "pause", "notes", "--kind", "gdrive", "--generation", "3")
	want := map[string]any{"project": "notes", "kind": "gdrive", "action": "pause", "generation": uint64(3)}
	if err != nil || name != "control_managed_backup" || !reflect.DeepEqual(params, want) {
		t.Fatalf("drive pause: %q %v %v", name, params, err)
	}
	// Without --kind the command controls the GitHub binding, as before.
	_, params, err = backupArgs(t, "backup", "run", "notes", "--generation", "2")
	if _, ok := params["kind"]; err != nil || ok || params["action"] != "now" {
		t.Fatalf("GitHub run: %v %v", params, err)
	}
	if _, _, err = backupArgs(t, "backup", "pause", "notes", "--kind", "s3", "--generation", "1"); err == nil {
		t.Fatal("an unknown destination kind was accepted")
	}
}
