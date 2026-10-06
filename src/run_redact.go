package bus

import (
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const redactedRunValue = "[redacted]"

var runSecretName = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|credential|authorization|api[-_]?key|access[-_]?key|private[-_]?key|cookie)`)
var runSecretAssignment = regexp.MustCompile(`(?i)([a-z0-9_-]*(?:password|passwd|secret|token|credential|authorization|api[-_]?key|access[-_]?key|private[-_]?key|cookie)[a-z0-9_-]*\s*[=:]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var runBearer = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[^\s"',;]+`)
var runKnownToken = regexp.MustCompile(`(?:\b(?:gh[pousr]_|github_pat_|sk-|xox[baprs]-|AIza|SG\.|npm_|pypi-|dop_v1_|hf_)[A-Za-z0-9_.-]+|\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]+|\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`)
var runURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
var runOpaqueToken = regexp.MustCompile(`[A-Za-z0-9_+/=-]{20,}`)

// Entropy is a heuristic for opaque credentials, not proof that a value is a
// secret. Hashes and random identifiers are deliberately hidden too.
func redactRunEntropy(s string) string {
	return runOpaqueToken.ReplaceAllStringFunc(s, func(token string) string {
		counts := make(map[rune]int)
		for _, c := range token {
			counts[c]++
		}
		entropy := 0.0
		for _, count := range counts {
			p := float64(count) / float64(len(token))
			entropy -= p * math.Log2(p)
		}
		hex := len(token) >= 24 && strings.Trim(token, "0123456789abcdefABCDEF") == ""
		if entropy >= 3.5 || hex && entropy >= 3 {
			return redactedRunValue
		}
		return token
	})
}

// redactRunArgs sanitizes the shared representation, never the argv executed.
// Inline programs can encode secrets in arbitrary ways, so hide their bodies.
// This is intentionally conservative; arbitrary unnamed secrets cannot be
// identified reliably and should not be passed as positional arguments.
func redactRunArgs(argv []string) []string {
	out := make([]string, len(argv))
	var secrets []string
	for _, env := range os.Environ() {
		key, value, ok := strings.Cut(env, "=")
		if ok && runSecretName.MatchString(key) && value != "" {
			secrets = append(secrets, value)
		}
	}
	hideNext, hideRest := false, false
	for i, arg := range argv {
		if hideNext || hideRest {
			out[i] = redactedRunValue
			hideNext = false
			continue
		}
		if i > 0 && (arg == "-c" || arg == "--command" || strings.EqualFold(arg, "-command") || strings.EqualFold(arg, "-encodedcommand") || arg == "-e" || arg == "--eval") {
			// Shells may place positional script secrets after the script body.
			hideRest = true
		}
		if i == 0 && (strings.ContainsAny(arg, "\n\r") || strings.HasPrefix(arg, "#!")) {
			out[i] = redactedRunValue
			continue
		}
		if strings.Contains(arg, "-----BEGIN") && strings.Contains(arg, "PRIVATE KEY-----") {
			out[i] = redactedRunValue
			continue
		}
		if i > 0 && strings.HasPrefix(arg, "-") && runSecretName.MatchString(strings.SplitN(arg, "=", 2)[0]) {
			if key, _, ok := strings.Cut(arg, "="); ok {
				arg = key + "=" + redactedRunValue
			} else {
				hideNext = true
			}
		}
		// Hide combined shell switches such as bash -lc SCRIPT.
		if i > 0 && strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c") {
			base := strings.ToLower(filepath.Base(argv[0]))
			if base == "sh" || base == "bash" || base == "zsh" || base == "dash" || base == "fish" || base == "ksh" {
				hideRest = true
			}
		}
		for _, secret := range secrets {
			arg = strings.ReplaceAll(arg, secret, redactedRunValue)
		}
		arg = runURL.ReplaceAllStringFunc(arg, func(raw string) string {
			u, err := url.Parse(raw)
			if err != nil {
				return redactedRunValue
			}
			if u.User != nil {
				u.User = url.User(redactedRunValue)
			}
			q := u.Query()
			for key := range q {
				if runSecretName.MatchString(key) {
					q.Set(key, redactedRunValue)
				}
			}
			u.RawQuery = q.Encode()
			if u.Fragment != "" {
				u.Fragment = redactedRunValue
			}
			return u.String()
		})
		arg = runBearer.ReplaceAllString(arg, "${1} "+redactedRunValue)
		arg = runSecretAssignment.ReplaceAllString(arg, "${1}"+redactedRunValue)
		out[i] = redactRunEntropy(runKnownToken.ReplaceAllString(arg, redactedRunValue))
	}
	return out
}
