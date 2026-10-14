package sops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/local/dockops/internal/config"
)

// fakeSops writes a shell script named "sops" into a fresh directory and
// prepends that directory to PATH (so the fake shadows a possibly installed
// real sops while cat and friends stay reachable). The fake records its
// invocation (one line of arguments, plus SOPS_AGE_KEY_FILE when set) into
// $SOPS_CALLS_FILE and then dumps the last argument — the encrypted file —
// to stdout, so the plaintext fixtures double as "decryption results". A
// missing input file makes cat fail with exit code 1 and stderr, which is
// exactly how the real sops reports it.
func fakeSops(t *testing.T) (binDir string, callsFile string) {
	t.Helper()
	binDir = t.TempDir()
	callsFile = filepath.Join(t.TempDir(), "calls.log")
	script := `#!/bin/sh
for last in "$@"; do :; done
if [ -n "$SOPS_CALLS_FILE" ]; then
	{ printf 'args:'; printf ' %s' "$@"; printf '\n'; } >> "$SOPS_CALLS_FILE"
	if [ -n "$SOPS_AGE_KEY_FILE" ]; then
		echo "SOPS_AGE_KEY_FILE=$SOPS_AGE_KEY_FILE" >> "$SOPS_CALLS_FILE"
	fi
fi
cat "$last"
`
	if err := os.WriteFile(filepath.Join(binDir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sops: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOPS_CALLS_FILE", callsFile)
	return binDir, callsFile
}

// writeFile creates path and parent directories with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readCalls(t *testing.T, callsFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(callsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read calls file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestDecryptFile(t *testing.T) {
	fakeSops(t)
	enc := filepath.Join(t.TempDir(), "secrets.env")
	writeFile(t, enc, "DB_PASSWORD=hunter2\n# comment\nGARBAGE LINE\nAPI_KEY=abc123\n")

	got, err := DecryptFile(context.Background(), enc)
	if err != nil {
		t.Fatalf("DecryptFile() error = %v", err)
	}
	want := map[string]string{"DB_PASSWORD": "hunter2", "API_KEY": "abc123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecryptFile() = %v, want %v", got, want)
	}
}

func TestDecryptFileArguments(t *testing.T) {
	_, callsFile := fakeSops(t)
	enc := filepath.Join(t.TempDir(), "secrets.env")
	writeFile(t, enc, "A=1\n")

	if _, err := DecryptFile(context.Background(), enc); err != nil {
		t.Fatalf("DecryptFile() error = %v", err)
	}
	calls := readCalls(t, callsFile)
	if len(calls) != 1 {
		t.Fatalf("sops called %d times, want exactly once: %v", len(calls), calls)
	}
	want := "args: -d --input-type dotenv --output-type dotenv " + enc
	if calls[0] != want {
		t.Errorf("sops invocation = %q, want %q", calls[0], want)
	}
}

func TestDecryptFileMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := DecryptFile(context.Background(), filepath.Join(t.TempDir(), "secrets.env"))
	if err == nil || !strings.Contains(err.Error(), "sops binary not found in PATH") {
		t.Errorf("DecryptFile() without binary error = %v, want PATH hint", err)
	}
}

func TestDecryptFileFailingBinary(t *testing.T) {
	fakeSops(t)
	// The fake's cat fails on a missing file with stderr output.
	_, err := DecryptFile(context.Background(), filepath.Join(t.TempDir(), "missing.env"))
	if err == nil || !strings.Contains(err.Error(), "missing.env") {
		t.Errorf("DecryptFile() on missing input error = %v, want path context", err)
	}
	if err != nil && !strings.Contains(err.Error(), "cat") {
		t.Errorf("DecryptFile() on missing input error = %v, want stderr snippet mentioning cat", err)
	}
}

func TestDecryptFilesMergesInOrder(t *testing.T) {
	fakeSops(t)
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "first.env"), "SHARED=first\nONLY_ONE=1\n")
	writeFile(t, filepath.Join(baseDir, "sub", "second.env"), "SHARED=second\nONLY_TWO=2\n")

	got, err := DecryptFiles(context.Background(), []config.SopsFile{
		{Path: "first.env"},
		{Path: "sub/second.env"}, // later file wins on key collisions
	}, baseDir)
	if err != nil {
		t.Fatalf("DecryptFiles() error = %v", err)
	}
	want := map[string]string{"SHARED": "second", "ONLY_ONE": "1", "ONLY_TWO": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecryptFiles() = %v, want %v", got, want)
	}
}

func TestDecryptFilesAbortsAtFirstError(t *testing.T) {
	_, callsFile := fakeSops(t)
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "ok.env"), "A=1\n")

	_, err := DecryptFiles(context.Background(), []config.SopsFile{
		{Path: "ok.env"},
		{Path: "broken.env"}, // does not exist — the fake sops fails
		{Path: "never.env"},  // must not be reached
	}, baseDir)
	if err == nil || !strings.Contains(err.Error(), "sops_files[1]") {
		t.Errorf("DecryptFiles() error = %v, want sops_files[1] context", err)
	}
	// The failing file is still decrypted (that is how the failure is
	// discovered), but nothing after it.
	calls := readCalls(t, callsFile)
	if len(calls) != 2 {
		t.Errorf("sops ran %d time(s), want exactly 2 (ok.env + broken.env): %v", len(calls), calls)
	}
	for _, call := range calls {
		if strings.Contains(call, "never.env") {
			t.Errorf("sops ran after a failing file: %v", calls)
		}
	}
}

func TestDecryptFilesAgeKeyfile(t *testing.T) {
	_, callsFile := fakeSops(t)
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "secrets.env"), "A=1\n")
	writeFile(t, filepath.Join(baseDir, "keys", "age.key"), "AGE-SECRET-KEY\n")

	if _, err := DecryptFiles(context.Background(), []config.SopsFile{
		{Path: "secrets.env", AgeKeyfile: "keys/age.key"},
	}, baseDir); err != nil {
		t.Fatalf("DecryptFiles() error = %v", err)
	}
	want := "SOPS_AGE_KEY_FILE=" + filepath.Join(baseDir, "keys", "age.key")
	calls := readCalls(t, callsFile)
	found := false
	for _, line := range calls {
		if line == want {
			found = true
		}
	}
	if !found {
		t.Errorf("sops child env missing %q; calls: %v", want, calls)
	}
}

func TestDecryptFilesAbsolutePathsPassThrough(t *testing.T) {
	fakeSops(t)
	enc := filepath.Join(t.TempDir(), "abs.env")
	writeFile(t, enc, "A=1\n")

	got, err := DecryptFiles(context.Background(), []config.SopsFile{{Path: enc}}, t.TempDir())
	if err != nil {
		t.Fatalf("DecryptFiles() error = %v", err)
	}
	if got["A"] != "1" {
		t.Errorf("A = %q, want 1 (absolute path must not be joined below baseDir)", got["A"])
	}
}

func TestDecryptFilesEmpty(t *testing.T) {
	got, err := DecryptFiles(context.Background(), nil, t.TempDir())
	if err != nil {
		t.Fatalf("DecryptFiles(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DecryptFiles(nil) = %v, want empty map", got)
	}
}
