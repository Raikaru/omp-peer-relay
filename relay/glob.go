package main

import (
	"errors"
	"regexp"
	"strings"
)

// Resources are either "task:<id>" or repo-relative POSIX paths/globs.
// `**` matches across directories, `*` and `?` stay within one segment.
// A literal path also covers everything beneath it (claiming "src/net"
// claims "src/net/x.go").

const taskPrefix = "task:"

// normalizeResource canonicalizes a claim resource and rejects anything that
// is not repo-relative, so both machines compare the same strings.
func normalizeResource(r string) (string, error) {
	r = strings.TrimSpace(r)
	if r == "" {
		return "", errors.New("empty resource")
	}
	if len(r) > 512 {
		return "", errors.New("resource too long")
	}
	if strings.HasPrefix(r, taskPrefix) {
		if len(r) == len(taskPrefix) {
			return "", errors.New("empty task id")
		}
		return r, nil
	}
	r = strings.ReplaceAll(r, `\`, "/")
	for strings.HasPrefix(r, "./") {
		r = r[2:]
	}
	r = strings.TrimSuffix(r, "/")
	if r == "" || r == "." {
		return "", errors.New("resource must name a path inside the repo, not the repo root")
	}
	if strings.HasPrefix(r, "/") || (len(r) > 1 && r[1] == ':') || strings.Contains(r, "://") {
		return "", errors.New("resource must be repo-relative: " + r)
	}
	for _, seg := range strings.Split(r, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return "", errors.New("resource must not contain empty, '.' or '..' segments: " + r)
		}
	}
	return r, nil
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?") }

// literalDir is the directory part of a glob before its first wildcard.
func literalDir(glob string) string {
	i := strings.IndexAny(glob, "*?")
	if i < 0 {
		return glob
	}
	prefix := glob[:i]
	if j := strings.LastIndex(prefix, "/"); j >= 0 {
		return prefix[:j]
	}
	return ""
}

// covers reports whether literal path a contains b (equal or ancestor).
func covers(a, b string) bool {
	return a == "" || a == b || strings.HasPrefix(b, a+"/")
}

func globRegexp(glob string) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			i++
			if i+1 < len(glob) && glob[i+1] == '/' {
				i++
				sb.WriteString("(?:.*/)?")
			} else {
				sb.WriteString(".*")
			}
		case c == '*':
			sb.WriteString("[^/]*")
		case c == '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	// A glob also covers descendants of anything it matches.
	sb.WriteString("(?:/.*)?$")
	return regexp.MustCompile(sb.String())
}

// overlaps reports whether two normalized resources may refer to the same
// file. Comparison is case-insensitive because Windows checkouts are.
// Glob-vs-glob is conservative: it may report overlap that a precise
// language-intersection test would not.
func overlaps(a, b string) bool {
	at, bt := strings.HasPrefix(a, taskPrefix), strings.HasPrefix(b, taskPrefix)
	if at || bt {
		return at && bt && strings.EqualFold(a, b)
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	ag, bg := hasWildcard(a), hasWildcard(b)
	switch {
	case !ag && !bg:
		return covers(a, b) || covers(b, a)
	case ag && !bg:
		return globRegexp(a).MatchString(b) || covers(b, literalDir(a))
	case !ag && bg:
		return globRegexp(b).MatchString(a) || covers(a, literalDir(b))
	default:
		la, lb := literalDir(a), literalDir(b)
		return covers(la, lb) || covers(lb, la)
	}
}
