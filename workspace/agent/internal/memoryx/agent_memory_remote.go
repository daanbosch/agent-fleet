package memoryx

import (
	"net/url"
	"regexp"
	"strings"
)

// agentMemNormalizeRemote reduces a git remote URL to "host/owner/repo" so the ssh, https and
// scp-like spellings of one repository compare equal (#2110). Credentials never survive: userinfo
// is dropped before anything is built, so a token in a URL cannot reach project.json. The port is
// dropped too (an ssh port and an https port name the same repository), the host is lower-cased
// and the path keeps its case. Anything that does not name a repository on a host (a local path,
// file://, a bare word) answers "": two checkouts of a path remote say nothing about each other.
//
// Only spellings that are known to name the same repository are unified. On a generic ssh server
// the path of "git@host:team/repo.git" is relative to the login user's home while
// "ssh://git@host/team/repo.git" is absolute, and "alice@host:x" and "git@host:x" can be different
// homes, so ssh and scp-like remotes are recorded only for the big forges, whose paths are
// forge-relative whatever the spelling (forgeHosts). Any other host is recorded for http(s)
// remotes alone; an ssh-only remote there has none, and no suggestion is made for it.
func agentMemNormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return ""
	}
	var host, path string
	ssh := false
	if i := strings.Index(raw, "://"); i > 0 {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
		case "ssh", "git+ssh", "ssh+git":
			ssh = true
		default:
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp-like: [user@]host:path. A "/" before the first ":" makes it a local path.
		colon := strings.Index(raw, ":")
		if colon <= 0 || strings.Contains(raw[:colon], "/") {
			return ""
		}
		host, path = raw[:colon], raw[colon+1:]
		ssh = true
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	host = strings.ToLower(host)
	if ssh && !forgeHosts[host] {
		return ""
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" {
		return ""
	}
	out := host + "/" + path
	if !agentMemRemoteOK(out) {
		return ""
	}
	return out
}

// forgeHosts are the hosts whose ssh and https remotes share one path form: owner/repo.
var forgeHosts = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true, "codeberg.org": true}

var agentMemRemoteRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]*(/[A-Za-z0-9._~-]+)+$`)

// agentMemRemoteOK says a string is a normalised remote as agentMemNormalizeRemote makes one. A
// project.json is read from a store anyone with a shell can edit, so the field is re-checked on
// the way in: it can never carry userinfo, a query, free text or something the secret scan flags
// (a token pasted into the path of an origin). The same check gates a freshly normalised value, so
// a flagged remote is never recorded, listed or shown; the project goes on without one.
func agentMemRemoteOK(s string) bool {
	return len(s) <= 300 && agentMemRemoteRe.MatchString(s) && len(agentMemScanText("project", s)) == 0
}
