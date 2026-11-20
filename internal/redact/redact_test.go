package redact

import "testing"

func TestURLs(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"user and token", "https://deploy:ghp_secret@github.com/x/y.git", "https://deploy:***@github.com/x/y.git"},
		{"token as user", "https://ghp_secret@github.com/x/y.git", "https://***@github.com/x/y.git"},
		{"oauth2 form", "https://oauth2:glpat-secret@gitlab.example.com/x.git", "https://oauth2:***@gitlab.example.com/x.git"},
		{"in an error text", "git clone --branch main -- https://u:p4ss@h/r.git /work/host: exit status 128: fatal: unable to access 'https://u:p4ss@h/r.git/'",
			"git clone --branch main -- https://u:***@h/r.git /work/host: exit status 128: fatal: unable to access 'https://u:***@h/r.git/'"},
		{"ssh user stays", "ssh://git@host:2222/x.git", "ssh://git@host:2222/x.git"},
		{"ssh password masked", "ssh://git:pw@host/x.git", "ssh://git:***@host/x.git"},
		{"scp style stays", "git@github.com:x/y.git", "git@github.com:x/y.git"},
		{"no url", "plain text with a@b mail", "plain text with a@b mail"},
		{"no credentials", "https://github.com/x/y.git", "https://github.com/x/y.git"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := URLs(tc.in); got != tc.want {
				t.Errorf("URLs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
