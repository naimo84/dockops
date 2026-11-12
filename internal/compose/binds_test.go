package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeStackFile writes one compose file into dir.
func writeStackFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestRewriteRelativeBindsTranslatesThroughMounts is the issue-#18 repro: a
// relative bind source in the container's stack directory must be rewritten to
// the host path of the same file, not passed through unchanged.
func TestRewriteRelativeBindsTranslatesThroughMounts(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/infrastructure/litellm"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  litellm:
    image: ghcr.io/x/litellm:latest
    volumes:
      - ./config.yaml:/app/config.yaml:ro
  db:
    image: postgres
    volumes:
      - pgdata:/var/lib/postgresql/data
`)
	roots := []BindRoot{
		{Container: "/var/lib/dockops/repos/infrastructure", Host: "/home/docker/infrastructure"},
		{Container: "/var/lib/dockops", Host: "/home/docker/dockops"},
	}
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, roots)
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	got := string(raw)
	want := "/home/docker/infrastructure/litellm/config.yaml:/app/config.yaml:ro"
	if !strings.Contains(got, want) {
		t.Errorf("override = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "./config.yaml") {
		t.Errorf("override = %q, want the relative source gone", got)
	}
	if strings.Contains(got, "pgdata") {
		t.Errorf("override = %q, want named volumes untouched", got)
	}
}

// TestRewriteRelativeBindsDeepestMountWins: the state volume and the checkout
// bind nest, so the shallow root must not win.
func TestRewriteRelativeBindsDeepestMountWins(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/app"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ../data/x.txt:/x.txt
`)
	roots := []BindRoot{
		{Container: "/var/lib/dockops", Host: "/home/docker/dockops"},
		{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"},
	}
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, roots)
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if want := "/home/docker/apps/data/x.txt:/x.txt"; !strings.Contains(string(raw), want) {
		t.Errorf("override = %q, want %q (deepest mount wins)", raw, want)
	}
}

// TestRewriteRelativeBindsNothingToDo: an absolute source is left alone — the
// documented workaround stays valid, it is just no longer required.
func TestRewriteRelativeBindsNothingToDo(t *testing.T) {
	stackDir := "/s"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - /srv/app/config.yaml:/app/config.yaml:ro
      - named:/data
`)
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/s", Host: "/host/s"}})
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 0 || raw != nil {
		t.Errorf("count = %d, raw = %q, want no override", count, raw)
	}
}

// TestRewriteRelativeBindsOutsideMountsIsAnError: a relative source dockops
// cannot translate is refused — the alternative is the silent empty-directory
// mount the whole feature exists to prevent.
func TestRewriteRelativeBindsOutsideMountsIsAnError(t *testing.T) {
	stackDir := "/opt/not-mounted/stack"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./config.yaml:/app/config.yaml:ro
`)
	_, _, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/var/lib/dockops", Host: "/home/docker/dockops"}})
	if err == nil {
		t.Fatal("RewriteRelativeBinds() error = nil, want a refusal")
	}
	for _, want := range []string{"./config.yaml", "outside every mount", "/opt/not-mounted/stack/config.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestRewriteRelativeBindsHostMode: without mounts (agent on the host) the
// container-absolute path already is the daemon's path — no translation, but
// still an absolute rewrite, which is what compose needs for -f consistency.
func TestRewriteRelativeBindsHostMode(t *testing.T) {
	stackDir := "/home/docker/apps/stack"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./conf.d:/etc/conf.d:ro
`)
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, nil)
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if want := "/home/docker/apps/stack/conf.d:/etc/conf.d:ro"; !strings.Contains(string(raw), want) {
		t.Errorf("override = %q, want %q", raw, want)
	}
}

// TestRewriteRelativeBindsLaterFileWins: compose merges volumes per target and
// the later file wins, so the override must state the effective source.
func TestRewriteRelativeBindsLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	base := writeStackFile(t, dir, "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./base.yaml:/app/config.yaml:ro
`)
	override := writeStackFile(t, dir, "prod.yaml", `
services:
  a:
    volumes:
      - ./prod.yaml:/app/config.yaml
`)
	raw, count, err := RewriteRelativeBinds([]string{base, override}, "/s", nil)
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 (one target, one effective source)", count)
	}
	got := string(raw)
	if !strings.Contains(got, "/s/prod.yaml:/app/config.yaml") {
		t.Errorf("override = %q, want the later file's source", got)
	}
	if strings.Contains(got, "base.yaml") {
		t.Errorf("override = %q, want the superseded source gone", got)
	}
}

// TestRewriteRelativeBindsLongSyntax: the long mapping form is resolved too,
// so a stack does not have to know which syntax dockops understands.
func TestRewriteRelativeBindsLongSyntax(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - type: bind
        source: ./cfg
        target: /etc/cfg
        read_only: true
`)
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}})
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if want := "/home/docker/apps/x/cfg:/etc/cfg:ro"; !strings.Contains(string(raw), want) {
		t.Errorf("override = %q, want %q", raw, want)
	}
}

// TestRewriteDocumentShape pins the YAML the override must have: the volumes
// under an explicit `volumes:` key. The first implementation rendered
// `services.<svc>: [- /x:/y]` (a bare sequence), which compose rejects with
// "volumes must be a mapping" — the override then failed the very deploy it
// was meant to repair.
func TestRewriteDocumentShape(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./cfg:/etc/cfg
`)
	raw, count, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}})
	if err != nil || count != 1 {
		t.Fatalf("RewriteRelativeBinds() = %d, %v; want 1, nil", count, err)
	}
	var doc struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("override is not the shape compose reads: %v\n%s", err, raw)
	}
	if got := doc.Services["a"].Volumes; len(got) != 1 || got[0] != "/home/docker/apps/x/cfg:/etc/cfg" {
		t.Errorf("override volumes = %v, want the rewritten entry under services.a.volumes", got)
	}
}

// TestRewriteDropsEmptyOptionSection: `source:target:` (trailing colon from an
// empty option string) is rejected by compose as "empty section between
// colons" — the entry has to end at the target when it carries no options.
func TestRewriteDropsEmptyOptionSection(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ./cfg:/etc/cfg
      - ./ro:/etc/ro:ro
`)
	raw, _, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}})
	if err != nil {
		t.Fatalf("RewriteRelativeBinds() error = %v", err)
	}
	if strings.Contains(string(raw), ":/etc/cfg:") {
		t.Errorf("override = %q, want no trailing empty option section", raw)
	}
	if !strings.Contains(string(raw), "/home/docker/apps/x/cfg:/etc/cfg\n") {
		t.Errorf("override = %q, want the optionless entry to end at the target", raw)
	}
	if !strings.Contains(string(raw), "/home/docker/apps/x/ro:/etc/ro:ro") {
		t.Errorf("override = %q, want the options of a short entry kept", raw)
	}
}

// TestRewriteRefusesInterpolatedSource: ${CONF_DIR:-./config} looks like a
// named volume to a naive classifier and would pass through untouched — the
// exact silent failure the rewrite exists to stop. The value is only known
// after interpolation, so the entry is an error, not a guess.
func TestRewriteRefusesInterpolatedSource(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - ${CONF_DIR:-./config}:/etc/cfg
`)
	_, _, err := RewriteRelativeBinds([]string{file}, stackDir, []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}})
	if err == nil || !strings.Contains(err.Error(), "interpolated") {
		t.Fatalf("err = %v, want a refusal for the interpolated source", err)
	}
	// A named volume with a $ would be caught too, but that is not valid
	// compose either — the rule errs on the loud side.
}

// TestTranslate: the pure mapping rules, including the exact-root case and the
// prefix trap (/var/lib/dockopsX must not match /var/lib/dockops).
func TestTranslate(t *testing.T) {
	roots := []BindRoot{
		{Container: "/var/lib/dockops", Host: "/home/docker/dockops"},
		{Container: "/srv/apps/", Host: "/home/docker/apps"},
	}
	cases := []struct{ in, want string }{
		{"/var/lib/dockops/state.json", "/home/docker/dockops/state.json"},
		{"/var/lib/dockops", "/home/docker/dockops"},
		{"/srv/apps/x/config.yaml", "/home/docker/apps/x/config.yaml"},
	}
	for _, c := range cases {
		got, ok := translate(c.in, roots)
		if !ok || got != c.want {
			t.Errorf("translate(%q) = %q, %v, want %q, true", c.in, got, ok, c.want)
		}
	}
	if _, ok := translate("/var/lib/dockopsX/y", roots); ok {
		t.Error("translate() matched a sibling directory sharing the prefix")
	}
	if _, ok := translate("/nowhere/x", roots); ok {
		t.Error("translate() matched an unrelated path")
	}
}

// TestNonPortableBindsReportsAbsoluteOutsideMounts is issue #18's suggestion 3:
// an absolute source the operator wrote themselves is passed through (they
// know the host namespace), but one outside every mount is still reported —
// it is the same silent empty-directory mount with the author's own path.
func TestNonPortableBindsReportsAbsoluteOutsideMounts(t *testing.T) {
	stackDir := "/var/lib/dockops/repos/host/x"
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  b:
    image: alpine
    volumes:
      - /home/docker/apps/x/good.conf:/etc/good.conf:ro
      - /nowhere/else/bad.conf:/etc/bad.conf:ro
      - ./rel.conf:/etc/rel.conf
      - named:/named
  a:
    image: alpine
    volumes:
      - /srv/other/deep.conf:/etc/deep.conf
`)
	roots := []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}}
	got, err := NonPortableBinds([]string{file}, stackDir, roots)
	if err != nil {
		t.Fatalf("NonPortableBinds() error = %v", err)
	}
	want := []string{
		"a: /srv/other/deep.conf (target /etc/deep.conf)",
		"b: /nowhere/else/bad.conf (target /etc/bad.conf)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NonPortableBinds() = %v, want %v (relative and mapped sources are not findings)", got, want)
	}
}

// TestNonPortableBindsHostModeStaysQuiet: without container mounts every path
// the agent names is in the daemon's namespace — reporting them would be a
// false alarm on every host-mode deployment.
func TestNonPortableBindsHostModeStaysQuiet(t *testing.T) {
	file := writeStackFile(t, t.TempDir(), "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - /srv/anywhere:/data
`)
	got, err := NonPortableBinds([]string{file}, "/srv/stack", nil)
	if err != nil {
		t.Fatalf("NonPortableBinds() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("NonPortableBinds() = %v, want none in host mode", got)
	}
}

// TestNonPortableBindsSeesEffectiveTarget pins that the report follows
// compose's merge rule: a later file that overrides the source of a target
// decides the finding, not the earlier one.
func TestNonPortableBindsSeesEffectiveTarget(t *testing.T) {
	dir := t.TempDir()
	first := writeStackFile(t, dir, "compose.yaml", `
services:
  a:
    image: alpine
    volumes:
      - /home/docker/apps/x/ok.conf:/etc/c.conf
`)
	second := writeStackFile(t, dir, "override.yaml", `
services:
  a:
    volumes:
      - /nowhere/c.conf:/etc/c.conf
`)
	roots := []BindRoot{{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"}}
	got, err := NonPortableBinds([]string{first, second}, dir, roots)
	if err != nil {
		t.Fatalf("NonPortableBinds() error = %v", err)
	}
	want := []string{"a: /nowhere/c.conf (target /etc/c.conf)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NonPortableBinds() = %v, want %v", got, want)
	}
}

// TestReverseTranslate: the host→container direction, including the prefix
// trap (a sibling directory must not match) and the exact-root case.
func TestReverseTranslate(t *testing.T) {
	roots := []BindRoot{
		{Container: "/var/lib/dockops/repos/host", Host: "/home/docker/apps"},
		{Container: "/var/lib/dockops", Host: "/home/docker/dockops"},
	}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/home/docker/apps/x/c.conf", "/var/lib/dockops/repos/host/x/c.conf", true},
		{"/home/docker/apps", "/var/lib/dockops/repos/host", true},
		// Sibling with the same prefix is not inside the mount.
		{"/home/docker/apps2/c.conf", "", false},
		{"/nowhere/c.conf", "", false},
	}
	for _, tc := range cases {
		got, ok := reverseTranslate(tc.in, roots)
		if ok != tc.ok || got != tc.want {
			t.Errorf("reverseTranslate(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
