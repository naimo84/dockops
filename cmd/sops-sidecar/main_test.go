package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRoots(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    []string
		wantErr string
	}{
		{
			name:  "single root",
			value: "/var/lib/dockops/repos",
			want:  []string{"/var/lib/dockops/repos"},
		},
		{
			name:  "colon-separated with empty entries",
			value: "/srv/repos::/srv/extra:",
			want:  []string{"/srv/repos", "/srv/extra"},
		},
		{
			name:  "redundant segments are cleaned",
			value: "/srv/repos/./sub/../",
			want:  []string{"/srv/repos"},
		},
		{
			name:    "relative root rejected",
			value:   "srv/repos",
			wantErr: "must be an absolute directory",
		},
		{
			name:    "empty value rejected",
			value:   "",
			wantErr: "must list at least one",
		},
		{
			name:    "only separators rejected",
			value:   ":::",
			wantErr: "must list at least one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRoots(tc.value)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseRoots(%q) error = %v, want error containing %q", tc.value, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRoots(%q) error = %v", tc.value, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseRoots(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseRootsUsesPathListSeparator(t *testing.T) {
	// The split follows the platform separator; on unix that is ":". The
	// assertion keeps the test meaningful if the suite ever runs elsewhere.
	sep := string(filepath.ListSeparator)
	got, err := parseRoots("/a" + sep + "/b")
	if err != nil {
		t.Fatalf("parseRoots() error = %v", err)
	}
	if want := []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseRoots() = %v, want %v", got, want)
	}
}
