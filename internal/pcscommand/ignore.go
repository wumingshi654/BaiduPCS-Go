package pcscommand

import (
    "bufio"
    "os"
    "path/filepath"
    "regexp"
    "strings"
)

type ignorePattern struct {
    raw     string
    neg     bool
    dirOnly bool
    anchored bool
    re      *regexp.Regexp
}

// loadPatternsForWatch loads and caches ignore patterns for a watch entry.
func loadPatternsForWatch(w *WatchEntry) ([]ignorePattern, error) {
    if w == nil {
        return nil, nil
    }
    if w.patterns != nil {
        return w.patterns, nil
    }

    // determine ignore file path
    ignorePath := w.IgnoreFile
    if ignorePath == "" {
        defaultPath := filepath.Join(w.Local, ".pcsignore")
        if _, err := os.Stat(defaultPath); err == nil {
            ignorePath = defaultPath
        } else {
            // no ignore file
            w.patterns = []ignorePattern{}
            return w.patterns, nil
        }
    } else {
        // if relative, join with local
        if !filepath.IsAbs(ignorePath) {
            ignorePath = filepath.Join(w.Local, ignorePath)
        }
    }

    f, err := os.Open(ignorePath)
    if err != nil {
        // treat as no patterns
        w.patterns = []ignorePattern{}
        return w.patterns, nil
    }
    defer f.Close()

    scanner := bufio.NewScanner(f)
    var patterns []ignorePattern
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }
        neg := false
        if strings.HasPrefix(line, "!") {
            neg = true
            line = strings.TrimSpace(line[1:])
            if line == "" {
                continue
            }
        }
        anchored := false
        if strings.HasPrefix(line, "/") {
            anchored = true
            line = strings.TrimPrefix(line, "/")
        }
        dirOnly := false
        if strings.HasSuffix(line, "/") {
            dirOnly = true
            line = strings.TrimSuffix(line, "/")
        }
        reStr := patternToRegexp(line, anchored)
        re, err := regexp.Compile(reStr)
        if err != nil {
            continue
        }
        patterns = append(patterns, ignorePattern{raw: line, neg: neg, dirOnly: dirOnly, anchored: anchored, re: re})
    }
    w.patterns = patterns
    return patterns, nil
}

// patternToRegexp converts a simplified gitignore pattern to a regexp string.
func patternToRegexp(p string, anchored bool) string {
    // convert pattern tokens
    var b strings.Builder
    for i := 0; i < len(p); {
        if i+1 < len(p) && p[i] == '*' && p[i+1] == '*' {
            b.WriteString(".*")
            i += 2
            continue
        }
        ch := p[i]
        if ch == '*' {
            b.WriteString("[^/]*")
        } else if ch == '?' {
            b.WriteString(".")
        } else {
            // escape regex special
            b.WriteString(regexp.QuoteMeta(string(ch)))
        }
        i++
    }
    core := b.String()
    if anchored {
        return "^" + core + "$"
    }
    // unanchored: allow match at any path segment
    return "^(.*/)?" + core + "$"
}

// shouldIgnore checks whether a relative path should be ignored according to patterns.
func shouldIgnore(w *WatchEntry, rel string, isDir bool) bool {
    patterns, _ := loadPatternsForWatch(w)
    if len(patterns) == 0 {
        return false
    }
    // normalize to unix-style
    rel = filepath.ToSlash(rel)
    ignored := false
    for _, p := range patterns {
        if p.dirOnly && !isDir {
            continue
        }
        if p.re.MatchString(rel) {
            if p.neg {
                ignored = false
            } else {
                ignored = true
            }
        }
    }
    return ignored
}
