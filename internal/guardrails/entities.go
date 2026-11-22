package guardrails

import (
	"encoding/base64"
	"encoding/json"
	"hash/crc32"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Word characters include letters and numbers in every script.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func validGitHub(value string) bool {
	if strings.HasPrefix(value, "github_pat_") {
		return true
	}
	if strings.HasPrefix(value, "ghs_") && strings.Contains(value[4:], "_") {
		_, token, ok := strings.Cut(value[4:], "_")
		return ok && validJWT(token)
	}
	if len(value) != 40 {
		return false
	}
	want := uint64(crc32.ChecksumIEEE([]byte(value[4:34])))
	for _, alphabet := range []string{"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		var n uint64
		for _, r := range value[34:] {
			i := strings.IndexRune(alphabet, r)
			if i < 0 {
				return false
			}
			n = n*62 + uint64(i)
		}
		if n == want {
			return true
		}
	}
	return false
}
func validGitLab(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) == 1 {
		return len(value) >= 26 && len(value) <= 70
	}
	if len(parts) != 3 || len(parts[1]) != 2 || len(parts[2]) != 9 {
		return false
	}
	length, err := strconv.ParseUint(parts[2][:2], 36, 16)
	if err != nil || int(length) != len(parts[0])-len("glpat-") {
		return false
	}
	checksum, err := strconv.ParseUint(parts[2][2:], 36, 32)
	if err != nil {
		return false
	}
	return uint32(checksum) == crc32.ChecksumIEEE([]byte(value[:len(value)-7]))
}

type entityTemplate struct {
	re    *regexp.Regexp
	check func(string) bool
}

var piiPatterns = map[string]entityTemplate{
	"pii.email":        {regexp.MustCompile(`[\p{L}\p{N}.!#$%&'*+/=?^_` + "`" + `{|}~-]{1,64}@[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?(?:\.[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?){1,10}`), nil},
	"pii.phone":        {regexp.MustCompile(`\+?[0-9(][0-9() .-]{5,28}[0-9]`), validPhone},
	"pii.payment_card": {regexp.MustCompile(`[0-9](?:[ -]?[0-9]){12,18}`), validLuhn},
	"pii.iban":         {regexp.MustCompile(`[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]){11,30}`), validIBAN},
	"pii.us_ssn":       {regexp.MustCompile(`[0-9]{3}[- ]?[0-9]{2}[- ]?[0-9]{4}`), validSSN},
	"pii.ip_address":   {regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}|[0-9A-Fa-f]{0,4}:[0-9A-Fa-f:]{1,40}`), func(v string) bool { _, err := netip.ParseAddr(v); return err == nil }},
}
var secretPatterns = map[string]entityTemplate{
	"secret.private_key":    {regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----[\s\S]+?-----END (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`), nil},
	"secret.jwt":            {regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), validJWT},
	"secret.aws_access_key": {regexp.MustCompile(`(?:AKIA|ASIA|AIDA|AROA)[A-Z0-9]{16}`), nil},
	"secret.google_cloud":   {regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), nil},
	"secret.azure":          {regexp.MustCompile(`(?i)(?:AccountKey=[A-Za-z0-9+/]{86}==|SharedAccessSignature[ =][^\r\n]+|[?&]sig=[A-Za-z0-9%+/=_-]{20,256})`), nil},
	"secret.github":         {regexp.MustCompile(`(?:ghs_[0-9]+_eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})`), validGitHub},
	"secret.gitlab":         {regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,300}(?:\.[0-9a-z]{2}\.[0-9a-z]{9})?`), validGitLab},
	"secret.slack":          {regexp.MustCompile(`(?:xox[baprs]-[A-Za-z0-9-]{10,200}|xapp-[A-Za-z0-9-]{10,200})`), nil},
	"secret.stripe":         {regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,128}`), nil},
	"secret.openai":         {regexp.MustCompile(`sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,200}`), func(v string) bool { return !strings.HasPrefix(v, "sk-ant-") }},
	"secret.anthropic":      {regexp.MustCompile(`sk-ant-(?:api[0-9]{2}-)?[A-Za-z0-9_-]{20,200}`), nil},
}

func digits(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, v)
}
func validPhone(v string) bool {
	d := digits(v)
	if strings.HasPrefix(v, "+") {
		return len(d) >= 8 && len(d) <= 15 && d[0] != '0'
	}
	return len(d) == 10 || (len(d) == 11 && d[0] == '1')
}
func validLuhn(v string) bool {
	v = digits(v)
	if len(v) < 13 || len(v) > 19 || strings.Trim(v, "0") == "" {
		return false
	}
	sum := 0
	for i := len(v) - 1; i >= 0; i-- {
		n := int(v[i] - '0')
		if (len(v)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// Country lengths: SWIFT IBAN Registry, release 103 (September 2026).
// https://www.swift.com/resource/iban-registry-pdf
var ibanLengths = map[string]int{"AL": 28, "AD": 24, "AT": 20, "AZ": 28, "BH": 22, "BE": 16, "BA": 20, "BR": 29, "BG": 22, "CR": 22, "HR": 21, "CY": 28, "CZ": 24, "DK": 18, "DO": 28, "EE": 20, "FO": 18, "FI": 18, "FR": 27, "GE": 22, "DE": 22, "GI": 23, "GR": 27, "GL": 18, "GT": 28, "HU": 28, "IS": 26, "IE": 22, "IL": 23, "IT": 27, "JO": 30, "KZ": 20, "XK": 20, "KW": 30, "LV": 21, "LB": 28, "LI": 21, "LT": 20, "LU": 20, "MT": 31, "MR": 27, "MU": 30, "MC": 27, "MD": 24, "ME": 22, "NL": 18, "MK": 19, "NO": 15, "PK": 24, "PS": 29, "PL": 28, "PT": 25, "QA": 29, "RO": 24, "SM": 27, "SA": 24, "RS": 22, "SK": 24, "SI": 19, "ES": 24, "SE": 24, "CH": 21, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "AE": 23, "GB": 22, "VA": 22, "BY": 28, "SV": 28, "SC": 31, "IQ": 23, "LC": 32, "ST": 25, "BI": 27, "DJ": 27, "EG": 29, "FK": 18, "HN": 28, "LY": 25, "MN": 20, "NI": 28, "OM": 23, "RU": 33, "SD": 18, "SO": 23, "VG": 24, "YE": 30}

func validIBAN(v string) bool {
	v = strings.ReplaceAll(v, " ", "")
	if len(v) < 15 || ibanLengths[v[:2]] != len(v) {
		return false
	}
	v = v[4:] + v[:4]
	n := 0
	for _, c := range v {
		if c >= '0' && c <= '9' {
			n = (n*10 + int(c-'0')) % 97
		} else if c >= 'A' && c <= 'Z' {
			n = (n*100 + int(c-'A') + 10) % 97
		} else {
			return false
		}
	}
	return n == 1
}
func validSSN(v string) bool {
	v = digits(v)
	return len(v) == 9 && !strings.HasPrefix(v, "000") && !strings.HasPrefix(v, "666") && v[0] != '9' && v[3:5] != "00" && v[5:] != "0000"
}
func validJWT(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(header, &h) != nil || h["alg"] == nil {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	return err == nil && json.Valid(body)
}
