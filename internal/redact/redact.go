// Package redact removes credentials from URLs before they reach logs, error
// texts, notifications or the status document. Repository URLs may carry an
// access token ("https://user:token@host/repo.git" is a documented way to
// authenticate HTTPS remotes), and git command lines and git's own output
// repeat the URL verbatim.
package redact

import (
	"regexp"
	"strings"
)

// userinfoPattern matches the scheme and userinfo of a URL inside free text:
// "<scheme>://<userinfo>@". The userinfo stops at characters that cannot be
// part of it unescaped, so the match never runs into the next word.
var userinfoPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)([^/@\s'"<>]+)@`)

// Mask replaces a credential.
const Mask = "***"

// URLs returns s with the credentials of every URL in it masked:
//
//	https://user:token@host/x  → https://user:***@host/x
//	https://token@host/x       → https://***@host/x   (token as user name)
//	ssh://git@host/x           → unchanged (a user name, no secret)
//
// A user name without password is masked for http(s) only, where tokens are
// commonly passed that way. scp-style remotes (git@host:repo) carry no
// credential and stay unchanged, as does every other text.
func URLs(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return userinfoPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := userinfoPattern.FindStringSubmatch(m)
		scheme, userinfo := parts[1], parts[2]
		if user, _, ok := strings.Cut(userinfo, ":"); ok {
			return scheme + user + ":" + Mask + "@"
		}
		switch strings.ToLower(scheme) {
		case "http://", "https://":
			return scheme + Mask + "@"
		}
		return m
	})
}
