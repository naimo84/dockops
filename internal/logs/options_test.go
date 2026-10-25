package logs

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseQuery(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		query   string
		stream  bool
		wantErr bool
		want    Options
	}{
		{name: "container defaults", query: "container=n8n-1", want: Options{Kind: KindContainer, Name: "n8n-1", Since: now.Add(-time.Hour), Tail: 200}},
		{name: "project defaults", query: "project=n8n", want: Options{Kind: KindProject, Name: "n8n", Since: now.Add(-time.Hour), Tail: 200}},
		{name: "full", query: "container=api-1&since=7d&tail=50&grep=ERROR&regex=1&format=json", want: Options{Kind: KindContainer, Name: "api-1", Since: now.Add(-7 * 24 * time.Hour), Tail: 50, Grep: "ERROR", Regex: true, Format: FormatJSON}},
		{name: "since rfc3339", query: "container=a&since=2026-10-25T10:30:00Z", want: Options{Kind: KindContainer, Name: "a", Since: time.Date(2026, 10, 25, 10, 30, 0, 0, time.UTC), Tail: 200}},
		{name: "since days and hours", query: "container=a&since=1d12h", want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-36 * time.Hour), Tail: 200}},
		{name: "since hours and minutes", query: "container=a&since=2h30m", want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-150 * time.Minute), Tail: 200}},
		{name: "stream defaults to tail 0", query: "container=a", stream: true, want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-time.Hour), Follow: true}},
		{name: "stream tail allowed", query: "container=a&tail=10", stream: true, want: Options{Kind: KindContainer, Name: "a", Since: now.Add(-time.Hour), Tail: 10, Follow: true}},

		{name: "neither", query: "", wantErr: true},
		{name: "both", query: "container=a&project=b", wantErr: true},
		{name: "container with leading dash", query: "container=-f", wantErr: true},
		{name: "container with double dash", query: "container=--help", wantErr: true},
		{name: "container with slash", query: "container=a%2Fb", wantErr: true},
		{name: "container with space", query: "container=a%20b", wantErr: true},
		{name: "container empty name", query: "container=.", wantErr: true},
		{name: "project with leading dash", query: "project=--help", wantErr: true},
		{name: "tail zero in snapshot", query: "container=a&tail=0", wantErr: true},
		{name: "tail negative", query: "container=a&tail=-3", wantErr: true},
		{name: "tail not a number", query: "container=a&tail=abc", wantErr: true},
		{name: "tail over maximum", query: "container=a&tail=5001", wantErr: true},
		{name: "since over maximum", query: "container=a&since=200h", wantErr: true},
		{name: "since rfc3339 too old", query: "container=a&since=2020-01-01T00:00:00Z", wantErr: true},
		{name: "since garbage", query: "container=a&since=yesterday", wantErr: true},
		{name: "since zero", query: "container=a&since=0s", wantErr: true},
		{name: "regex without grep", query: "container=a&regex=1", wantErr: true},
		{name: "regex broken", query: "container=a&grep=%28&regex=1", wantErr: true},
		{name: "regex value invalid", query: "container=a&grep=x&regex=yes", wantErr: true},
		{name: "grep too long", query: "container=a&grep=" + strings.Repeat("x", MaxGrepLen+1), wantErr: true},
		{name: "format invalid", query: "container=a&format=xml", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			var (
				got  Options
				perr error
			)
			if tc.stream {
				got, perr = ParseStreamQuery(values, now)
			} else {
				got, perr = ParseQuery(values, now)
			}
			if tc.wantErr {
				if perr == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected error: %v", perr)
			}
			if got != tc.want {
				t.Errorf("Options = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestComposeRendering(t *testing.T) {
	since := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	container := Options{Kind: KindContainer, Name: "n8n-1", Since: since, Tail: 5}.Compose()
	if container.Container != "n8n-1" || container.Project != "" || container.Tail != 5 {
		t.Errorf("container options = %+v", container)
	}
	project := Options{Kind: KindProject, Name: "n8n", Since: since, Follow: true}.Compose()
	if project.Project != "n8n" || project.Container != "" || !project.Follow {
		t.Errorf("project options = %+v", project)
	}
}
