package config

import (
	"reflect"
	"testing"
)

// restart_files concatenate like env_files: the base stack names its own
// config, a host import can add a host-specific one.
func TestApplyImportRestartFiles(t *testing.T) {
	base := Stack{Name: "traefik", RestartFiles: []string{"config.yaml"}}
	imp := Import{RestartFiles: []string{"config.yaml", "dynamic/host.yaml"}}
	got := applyImport(base, imp).RestartFiles
	if want := []string{"config.yaml", "dynamic/host.yaml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RestartFiles = %v, want %v", got, want)
	}
}
