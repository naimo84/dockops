// Reading (not just driving) compose state: the Ps* methods power the
// monitoring endpoint GET /status (internal/webhook) — the one place where
// dockops deliberately inspects the running state instead of the desired one.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// composeProjectLabel is the container label docker compose stamps on every
// resource it creates; its value is the project name (-p).
const composeProjectLabel = "com.docker.compose.project"

// Container is one row of docker (compose) ps output, normalized to the
// fields /status reports. State holds values like "running", "exited",
// "created" or "paused"; Health is the healthcheck verdict ("healthy",
// "unhealthy", "starting") or empty when the service has no healthcheck.
type Container struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Health string `json:"health"`
}

// UnmarshalJSON accepts the key spellings different CLI versions emit: modern
// compose reports "Name", docker ps reports "Names", and some versions report
// both or only the plural. The lowercase tags of Container match
// case-insensitively; the plural alias is resolved here.
func (c *Container) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name   string `json:"Name"`
		Names  string `json:"Names"`
		State  string `json:"State"`
		Health string `json:"Health"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Name = raw.Name
	if c.Name == "" {
		c.Name = raw.Names
	}
	c.State = raw.State
	c.Health = raw.Health
	return nil
}

// ProjectRef names a compose project among the running containers, with the
// container names belonging to it. Projects are reported sorted by name.
type ProjectRef struct {
	Project    string
	Containers []string
}

// PsProject lists all containers — running and stopped — of a compose
// project:
//
//	<binary> compose -p <project> ps --all --format json
//
// The --format json output differs between compose versions: newer ones print
// a JSON array, older ones one JSON object per line; both shapes are parsed
// tolerantly (a lone object passes as well). A project without containers
// yields an empty slice, not an error.
func (e *Executor) PsProject(ctx context.Context, project string) ([]Container, error) {
	args := []string{"compose", "-p", project, "ps", "--all", "--format", "json"}
	out, err := e.output(ctx, args)
	if err != nil {
		return nil, err
	}
	containers, err := parseContainers(out)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return containers, nil
}

// PsAllProjects lists the compose projects with running containers on the
// host:
//
//	<binary> ps --format {{json .}}
//
// The template form (instead of the newer plain "json") works on every docker
// CLI version and emits one JSON object per container. Containers without the
// com.docker.compose.project label are skipped on purpose — they are foreign
// processes (buildkit, manually started containers, …), reporting them as
// compose projects would be noise. The label value's encoding varies by
// version (comma-joined string, array or object); all three are understood.
func (e *Executor) PsAllProjects(ctx context.Context) ([]ProjectRef, error) {
	args := []string{"ps", "--format", "{{json .}}"}
	out, err := e.output(ctx, args)
	if err != nil {
		return nil, err
	}
	refs, err := parseProjectRefs(out)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return refs, nil
}

// parseContainers decodes compose ps output: a JSON array (modern compose)
// or JSON lines (older versions), tolerating a lone object and empty output.
func parseContainers(out []byte) ([]Container, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var arr []Container
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		return arr, nil
	}
	// Not an array: decode consecutive JSON values (JSON lines, or a single
	// object). A decoding error other than EOF means unusable output.
	var containers []Container
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var c Container
		err := dec.Decode(&c)
		if errors.Is(err, io.EOF) {
			return containers, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse ps output: %w", err)
		}
		containers = append(containers, c)
	}
}

// parseProjectRefs decodes docker ps rows into compose projects with their
// container names, skipping rows without the compose project label.
func parseProjectRefs(out []byte) ([]ProjectRef, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// docker ps emits JSON lines; a wrapping array is tolerated the same way
	// as in parseContainers, so future CLI changes cannot break the endpoint.
	rows, err := parseRows(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse ps output: %w", err)
	}
	byProject := make(map[string][]string)
	for _, row := range rows {
		project := labelValue(row.Labels, composeProjectLabel)
		if project == "" {
			continue // not compose-managed: buildkit, manual runs, …
		}
		byProject[project] = append(byProject[project], row.Name)
	}
	refs := make([]ProjectRef, 0, len(byProject))
	for project, names := range byProject {
		sort.Strings(names)
		refs = append(refs, ProjectRef{Project: project, Containers: names})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Project < refs[j].Project })
	return refs, nil
}

// psRow is one docker ps row reduced to the fields the project discovery
// needs. Labels arrives as raw JSON because its encoding varies by version.
type psRow struct {
	Name   string          `json:"Names"`
	Labels json.RawMessage `json:"Labels"`
}

// parseRows decodes ps output of either shape (array or JSON lines) into rows.
func parseRows(trimmed []byte) ([]psRow, error) {
	var arr []psRow
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		return arr, nil
	}
	var rows []psRow
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for {
		var row psRow
		err := dec.Decode(&row)
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
}

// labelValue extracts the value of key from a docker ps Labels field, which
// CLI versions encode differently: a comma-joined "k=v" string, an array of
// "k=v" entries or a JSON object. Anything else yields "" (the label counts
// as absent).
func labelValue(labels json.RawMessage, key string) string {
	if len(labels) == 0 {
		return ""
	}
	var entries []string
	var joined string
	if err := json.Unmarshal(labels, &joined); err == nil {
		if joined == "" {
			return ""
		}
		entries = strings.Split(joined, ",")
	} else {
		var arr []string
		if err := json.Unmarshal(labels, &arr); err == nil {
			entries = arr
		} else {
			var obj map[string]string
			if err := json.Unmarshal(labels, &obj); err == nil {
				return obj[key]
			}
			return ""
		}
	}
	prefix := key + "="
	for _, entry := range entries {
		if value, ok := strings.CutPrefix(strings.TrimSpace(entry), prefix); ok {
			return value
		}
	}
	return ""
}

// output executes the composed command line and returns its stdout. Failures
// are logged and returned like run does, but with stderr in the message (the
// useful diagnostics for a failed ps; stdout stays unpolluted for parsing).
func (e *Executor) output(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	command := strings.Join(args, " ")
	if err := cmd.Run(); err != nil {
		e.logger.Error("docker command failed",
			"command", command, "error", err.Error(), "output", snippet(stderr.Bytes()))
		return nil, fmt.Errorf("docker %s: %w%s", command, err, outputSuffix(stderr.Bytes()))
	}
	e.logger.Info("docker command finished", "command", command, "output", snippet(stdout.Bytes()))
	return stdout.Bytes(), nil
}
