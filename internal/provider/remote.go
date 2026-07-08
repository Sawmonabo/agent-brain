package provider

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeRemoteURL canonicalizes a git remote to the machine-independent
// project id "host/owner/repo" (spec §3). Credentials embedded in https
// URLs are stripped and never appear in the id. Local-only remotes
// (file://, plain paths) are rejected — they cannot identify a project
// across machines.
func NormalizeRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty remote url")
	}
	// scp-like syntax: git@host:owner/repo(.git)
	if !strings.Contains(raw, "://") {
		at := strings.Index(raw, "@")
		colon := strings.Index(raw, ":")
		if at >= 0 && colon > at {
			host, path := raw[at+1:colon], raw[colon+1:]
			return joinRemoteID(host, path)
		}
		return "", fmt.Errorf("remote %q is not a recognizable git url", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse remote url: %w", err)
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
		return joinRemoteID(u.Hostname(), u.Path)
	default:
		return "", fmt.Errorf("remote scheme %q cannot identify a project across machines", u.Scheme)
	}
}

func joinRemoteID(host, path string) (string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" || !strings.Contains(path, "/") {
		return "", fmt.Errorf("remote host/path %q/%q incomplete", host, path)
	}
	return strings.ToLower(host) + "/" + path, nil
}
