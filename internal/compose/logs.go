// Reading container logs and the container inventory for the HTTP log API
// (internal/webhook/logs.go). Like ps.go, this is a read-only exception to
// the "compose drives deployments" rule — the log API reports what runs, it
// never changes it.
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
	"strconv"
	"strings"
	"time"
)

// dockerPsRow is one line of `docker ps -a --format {{json .}}`. Only the
// fields the log API reports are decoded; everything else is ignored on
// purpose — the exact shape of that output is a CLI detail, not a contract.
type dockerPsRow struct {
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

// ContainerInfo is one row of the container inventory served by
// GET /logs/containers. Project and Service are empty for containers docker
// compose did not create; Health is empty when the container has no
// healthcheck.
type ContainerInfo struct {
	Name    string `json:"container"`
	Project string `json:"project"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Health  string `json:"health"`
	Created string `json:"created"`
}

// PsContainers lists every container of the host, running or not:
//
//	<binary> ps -a --format {{json .}}
//
// The template form works on every docker CLI version and emits one JSON
// object per line. Rows are sorted by project, service, container; rows
// without a compose project come last.
func (e *Executor) PsContainers(ctx context.Context) ([]ContainerInfo, error) {
	out, err := e.output(ctx, []string{"ps", "-a", "--format", "{{json .}}"})
	if err != nil {
		return nil, err
	}
	infos := make([]ContainerInfo, 0, 16)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerPsRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("docker ps -a --format {{json .}}: %w", err)
		}
		if row.Names == "" {
			continue
		}
		info := ContainerInfo{
			Name:    row.Names,
			Image:   row.Image,
			State:   row.State,
			Health:  healthFromStatus(row.Status),
			Created: normalizeCreatedAt(row.CreatedAt),
		}
		for _, label := range strings.Split(row.Labels, ",") {
			key, value, ok := strings.Cut(label, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case composeProjectLabel:
				info.Project = value
			case composeServiceLabel:
				info.Service = value
			}
		}
		infos = append(infos, info)
	}
	sort.SliceStable(infos, func(i, j int) bool {
		a, b := infos[i], infos[j]
		if (a.Project == "") != (b.Project == "") {
			return b.Project == ""
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	return infos, nil
}

// composeServiceLabel is the label docker compose stamps on the service name.
const composeServiceLabel = "com.docker.compose.service"

// healthFromStatus extracts the healthcheck verdict docker renders into the
// human-readable Status column ("Up 2 hours (unhealthy)"); empty when the
// container has no healthcheck.
func healthFromStatus(status string) string {
	for _, verdict := range []string{"healthy", "unhealthy", "starting"} {
		if strings.Contains(status, "("+verdict+")") {
			return verdict
		}
	}
	return ""
}

// normalizeCreatedAt turns the human-format CreatedAt ("2026-10-20 09:12:00
// +0200 CEST") into RFC3339; unparseable input degrades to "" rather than
// failing the whole inventory.
func normalizeCreatedAt(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := time.Parse("2006-01-02 15:04:05 -0700 MST", raw)
	if err != nil {
		return ""
	}
	return parsed.Format(time.RFC3339)
}

// LogsOptions describes one log read. Exactly one of Container and Project is
// set: a single container, or every container of a compose project (docker
// compose merges and prefixes those itself). Tail 0 omits --tail — a follow
// mode stream then starts with new lines only. Since is always sent.
type LogsOptions struct {
	Container string
	Project   string
	Since     time.Time
	Tail      int
	Follow    bool
}

// Logs writes the container's (or project's) log stream to w:
//
//	<binary> logs --timestamps --since <t> [--tail n] [--follow] <container>
//	<binary> compose -p <project> logs --no-color --timestamps --since <t> [--tail n] [--follow]
//
// w is written as the process produces output, so follow mode streams
// straight through. Filters and limits live in internal/logs.
func (e *Executor) Logs(ctx context.Context, opts LogsOptions, w io.Writer) error {
	args, err := logArgs(opts)
	if err != nil {
		return err
	}
	return e.stream(ctx, args, w)
}

// logArgs builds the command line; the container name is the last positional
// argument, every flag precedes it.
func logArgs(o LogsOptions) ([]string, error) {
	switch {
	case o.Container != "" && o.Project != "":
		return nil, errors.New("container and project are mutually exclusive")
	case o.Container == "" && o.Project == "":
		return nil, errors.New("one of container or project is required")
	}
	since := o.Since.UTC().Format(time.RFC3339)
	var args []string
	if o.Project != "" {
		args = []string{"compose", "-p", o.Project, "logs", "--no-color", "--timestamps", "--since", since}
	} else {
		args = []string{"logs", "--timestamps", "--since", since}
	}
	if o.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(o.Tail))
	}
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Container != "" {
		args = append(args, o.Container)
	}
	return args, nil
}

// stream runs the composed command and copies stdout to w. Unlike output() it
// does not buffer stdout — log output can be arbitrarily large — but it does
// capture stderr, so docker's own diagnosis ("No such container",
// "configured logging driver does not support reading") survives into the
// error and can be mapped to an HTTP status by the caller. WaitDelay
// guarantees a SIGKILL five seconds after the context is cancelled, so a
// follow-mode subprocess can never outlive its request.
func (e *Executor) stream(ctx context.Context, args []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	command := strings.Join(args, " ")
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		e.logger.Error("docker command failed",
			"command", command, "error", err.Error(), "output", snippet(stderr.Bytes()))
		return fmt.Errorf("docker %s: %w%s", command, err, outputSuffix(stderr.Bytes()))
	}
	return nil
}
