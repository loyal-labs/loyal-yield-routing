package webapp

// Shared text and token contracts for the observability ingest routes, porting
// web/apps/web/src/features/observability/error-contract.ts,
// trace-context.ts and the parts of @loyal-labs/shared they lean on. All
// length bounds count UTF-16 code units like the TS string .length.

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	obsMaxErrorNameLength     = 80
	obsMaxErrorMessageLength  = 512
	obsMaxErrorStackLength    = 4096
	obsMaxReleaseLength       = 80
	obsMaxEnvironmentLength   = 32
	obsMaxPathnameLength      = 256
	obsMaxRawFieldLength      = 12 * 1024
	obsMaxRequestBytes        = 16 * 1024
	obsMaxExportBudget        = 1250
	obsMaxEventAgeMS          = 60 * 60 * 1000
	obsMaxEventClockSkewMS    = 5 * 60 * 1000
	obsInternalWalletDefault  = "BmzZz5VTNU57EHaGwyDcP1vXz9a8RwEDYAcPCdW4nPg1"
	obsMaxLifecycleMessageLen = 256
)

var errObservabilityTooLarge = errors.New("Observability request is too large.")

var errObservabilityEnvelope = errors.New("Invalid observability envelope.")

func truncateUTF16(s string, maxLength int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > maxLength {
			return s[:i]
		}
		units += width
	}
	return s
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

var (
	obsURLQueryPattern = regexp.MustCompile(`([?&][^=\s&#]{1,64}=)[^&#\s]*`)
	obsBearerPattern   = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	obsHeaderPattern   = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie)\b\s*[:=]\s*[^\n]*`)
	obsSecretPattern   = regexp.MustCompile(`(?i)\b(api[-_ ]?key|authorization|cookie|password|secret|session|token)\b(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
	obsBodyPattern     = regexp.MustCompile(`(?i)\b(request body|response body|request headers|response headers)\b\s*[:=]\s*[^\n]*`)
)

func sanitizeTelemetryText(value string, maxLength int) string {
	redacted := obsURLQueryPattern.ReplaceAllString(value, "${1}[REDACTED]")
	redacted = obsBearerPattern.ReplaceAllString(redacted, "Bearer [REDACTED]")
	redacted = obsHeaderPattern.ReplaceAllString(redacted, "${1}=[REDACTED]")
	redacted = obsSecretPattern.ReplaceAllString(redacted, "${1}${2}[REDACTED]")
	redacted = obsBodyPattern.ReplaceAllString(redacted, "${1}=[REDACTED]")
	redacted = redactLongRun(redacted, obsBase58Run, 32, "")
	redacted = redactLongRun(redacted, obsHexRun, 32, "0x")
	redacted = redactLongRun(redacted, obsEncodedRun, 64, "")
	return truncateUTF16(redacted, maxLength)
}

// The three identifier redactions use lookaheads JS supports and RE2 does not,
// so each is a maximal-run scan that reproduces the JS left-to-right replace:
// a run qualifies at [start,end) when it is long enough, the character before
// it (or string start) plays the (^|[^...]) group, and any literal prefix the
// pattern may consume ("0x") sits directly before the run behind a non-run
// character.
type obsRunAlphabet struct {
	match func(r rune) bool
}

var (
	obsBase58Run = obsRunAlphabet{match: func(r rune) bool {
		return r >= '1' && r <= '9' || r >= 'A' && r <= 'H' || r >= 'J' && r <= 'N' || r >= 'P' && r <= 'Z' ||
			r >= 'a' && r <= 'k' || r >= 'm' && r <= 'z'
	}}
	obsHexRun = obsRunAlphabet{match: func(r rune) bool {
		return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
	}}
	obsEncodedRun = obsRunAlphabet{match: func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '+' || r == '/' || r == '=' || r == '-'
	}}
)

func redactLongRun(s string, alphabet obsRunAlphabet, minLen int, prefix string) string {
	var out strings.Builder
	out.Grow(len(s))
	runStart := -1
	segmentStart := 0
	for i := 0; i <= len(s); {
		if r, width := runeAt(s, i); i < len(s) && alphabet.match(r) {
			if runStart < 0 {
				runStart = i
			}
			i += width
			continue
		}
		if runStart >= 0 && i-runStart >= minLen {
			groupStart := runStart
			groupLen := 1 // the (^|[^...]) character, except at string start
			if prefix != "" && runStart-len(prefix) >= 0 && s[runStart-len(prefix):runStart] == prefix {
				if runStart == len(prefix) {
					groupStart, groupLen = 0, 0 // (^) empty group at string start
				} else if r, width := runeLast(s, runStart-len(prefix)); !alphabet.match(r) {
					groupStart, groupLen = runStart-len(prefix)-width, width+1
				} else {
					// The prefix is preceded by an alphabet character, so the
					// JS regex cannot match there; the plain run behind the
					// prefix's last byte (never in the alphabet) matches.
					groupStart, groupLen = runStart-1, 1
				}
			} else if runStart == 0 {
				groupStart, groupLen = 0, 0
			}
			out.WriteString(s[segmentStart:groupStart])
			out.WriteString(s[groupStart : groupStart+groupLen])
			out.WriteString("[REDACTED_IDENTIFIER]")
			segmentStart = i
		}
		runStart = -1
		if i >= len(s) {
			break
		}
		_, width := runeAt(s, i)
		i += width
	}
	if segmentStart < len(s) {
		out.WriteString(s[segmentStart:])
	}
	return out.String()
}

func runeAt(s string, i int) (rune, int) {
	if i >= len(s) {
		return 0, 0
	}
	r, width := utf8.DecodeRuneInString(s[i:])
	if width == 0 {
		return rune(s[i]), 1
	}
	return r, width
}

func runeLast(s string, end int) (rune, int) {
	r, width := utf8.DecodeLastRuneInString(s[:end])
	return r, width
}

var obsResourceValuePattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func normalizeResourceValue(value string, maxLength int) string {
	normalized := obsResourceValuePattern.ReplaceAllString(value, "_")
	normalized = truncateUTF16(normalized, maxLength)
	if normalized == "" {
		return ""
	}
	return normalized
}

var (
	obsControlPattern    = regexp.MustCompile(`[\x{0000}-\x{001f}\x{007f}]`)
	obsUUIDV4Pattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	obsTraceIDPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	obsAllZeroPattern    = regexp.MustCompile(`^0+$`)
	obsSpanIDPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	obsWalletAddPattern  = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	obsScheduledSlot     = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	obsClientBuildID     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	obsPageSessionID     = obsUUIDV4Pattern
	obsSlotIDPatternName = "scheduledSlotId"
)

func isCanonicalUUIDV4(value any) bool {
	s, ok := value.(string)
	return ok && obsUUIDV4Pattern.MatchString(s)
}

func isTraceID(value any) bool {
	s, ok := value.(string)
	return ok && obsTraceIDPattern.MatchString(s) && !obsAllZeroPattern.MatchString(s)
}

func isSpanID(value any) bool {
	s, ok := value.(string)
	return ok && obsSpanIDPattern.MatchString(s) && !obsAllZeroPattern.MatchString(s)
}

func normalizeTelemetryPathname(value string) string {
	if value == "" || utf16Len(value) > obsMaxRawFieldLength ||
		!strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.Contains(value, "\\") || obsControlPattern.MatchString(value) {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return ""
	}
	path := removeDotSegments(parsed.EscapedPath())
	return sanitizeTelemetryText(path, obsMaxPathnameLength)
}

// removeDotSegments applies RFC 3986 section 5.2.4, which matches the WHATWG
// URL path normalization new URL(value, base) performs for absolute paths.
func removeDotSegments(path string) string {
	if !strings.Contains(path, ".") {
		return path
	}
	out := strings.Builder{}
	out.Grow(len(path))
	input := path
	for input != "" {
		switch {
		case strings.HasPrefix(input, "../"):
			input = input[3:]
		case strings.HasPrefix(input, "./"):
			input = input[2:]
		case strings.HasPrefix(input, "/./"):
			input = input[2:]
		case input == "/.":
			input = "/"
		case strings.HasPrefix(input, "/../"):
			input = input[3:]
			cutLastSegment(&out)
		case input == "/..":
			input = "/"
			cutLastSegment(&out)
		case input == "." || input == "..":
			input = ""
		default:
			start := 0
			if input[0] == '/' {
				start = strings.IndexByte(input[1:], '/')
				if start < 0 {
					start = len(input)
				} else {
					start++
				}
			} else if idx := strings.IndexByte(input, '/'); idx >= 0 {
				start = idx
			} else {
				start = len(input)
			}
			out.WriteString(input[:start])
			input = input[start:]
		}
	}
	return out.String()
}

func cutLastSegment(out *strings.Builder) {
	s := out.String()
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		out.Reset()
		out.WriteString(s[:idx])
	}
}
