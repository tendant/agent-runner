package gitsafe

import (
	"net/url"
	"strings"
)

// RedactURL hides the credential in a remote URL before it is logged or
// shown in chat: https://user:token@host/repo becomes
// https://user:xxxxx@host/repo; a token-only userinfo
// (https://token@host/repo) is hidden too. Anything that is not such a URL
// (scp-style git@host:repo, a path) comes back as it was.
func RedactURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.User == nil || u.Host == "" {
		return s
	}
	if _, ok := u.User.Password(); !ok {
		// https://TOKEN@host: the name is the credential. ssh://git@host is
		// an ordinary user name.
		if u.Scheme == "ssh" || u.User.Username() == "git" {
			return s
		}
		u.User = url.User("xxxxx")
		return u.String()
	}
	return u.Redacted()
}

// HasCredential reports whether a remote URL carries a password or token.
func HasCredential(s string) bool { return RedactURL(s) != s }
