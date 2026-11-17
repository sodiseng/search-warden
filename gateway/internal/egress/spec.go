// Package egress models outbound proxies ("egresses") and the rules that let a
// key reference one either by node name or by an inline URL (ARCHITECTURE.md §7.9).
//
// Identity rule: an egress is identified by its *normalized URL*. Names are
// aliases. A key that writes the same proxy inline and a node that lists it
// therefore share one egress ID, one health state and one key count.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Direct is the built-in egress meaning "the server's own IP".
const Direct = "direct"

// DirectID is the egress ID of Direct.
const DirectID = "direct"

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateName enforces §7.9.2 rule 2: names never look like URLs and never shadow "direct".
func ValidateName(name string) error {
	switch {
	case strings.EqualFold(name, Direct):
		return fmt.Errorf("egress name %q is reserved", name)
	case strings.Contains(name, "://"):
		return fmt.Errorf("egress name %q must not contain \"://\"", name)
	case net.ParseIP(name) != nil:
		return fmt.Errorf("egress name %q looks like an IP address; write the proxy as socks5://%s:<port> or pick another name", name, name)
	case !nameRe.MatchString(name):
		return fmt.Errorf("egress name %q: use 1-64 chars of letters, digits, '-', '_', '.', starting with a letter or digit", name)
	}
	return nil
}

var defaultPorts = map[string]string{"socks5": "1080", "http": "80", "https": "443"}

// NormalizeURL canonicalizes a proxy URL so equal proxies compare equal:
// lower-case scheme and host, socks5h→socks5 (same behaviour in Go: DNS is
// resolved by the proxy), default port removed, trailing "/" removed.
// Credentials are kept verbatim and take part in equality.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", redactErr(err, raw))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	def, ok := defaultPorts[scheme]
	if !ok {
		return "", fmt.Errorf("unsupported proxy scheme %q (use socks5, socks5h, http or https)", u.Scheme)
	}
	if u.Opaque != "" || u.Host == "" {
		return "", errors.New("proxy URL needs a host, e.g. socks5://1.2.3.4:1080")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("proxy URL must not have a path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("proxy URL needs a host")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid proxy port %q", port)
		}
		port = strconv.Itoa(n) // strip leading zeros
	}
	if port == def {
		port = ""
	}
	hostport := host
	if strings.Contains(host, ":") { // IPv6
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}
	out := url.URL{Scheme: scheme, User: u.User, Host: hostport}
	return out.String(), nil
}

// WithPort renders a normalized URL with its port spelled out, for writing
// into files (socks5://h → socks5://h:1080). Identity still uses NormalizeURL.
func WithPort(normalized string) string {
	u, err := url.Parse(normalized)
	if err != nil || u.Port() != "" {
		return normalized
	}
	if p, ok := defaultPorts[u.Scheme]; ok {
		u.Host = net.JoinHostPort(u.Hostname(), p)
	}
	return u.String()
}

// redactErr keeps credentials out of url.Parse error messages.
func redactErr(err error, raw string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	_ = raw
	return err
}

// ID derives the stable egress ID from a normalized URL. Renaming a node keeps
// its ID (and therefore its health/exit-IP state); changing its URL does not.
func ID(normalizedURL string) string {
	if normalizedURL == "" || normalizedURL == Direct {
		return DirectID
	}
	sum := sha256.Sum256([]byte(normalizedURL))
	return "e" + hex.EncodeToString(sum[:])[:12]
}

// Redact masks the password: socks5://user:***@host:port. Safe for logs and lists.
func Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid url>"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return strings.Replace(u.Redacted(), "xxxxx", "***", 1)
		}
	}
	return u.String()
}

// RefKind classifies the value of a key's egress field.
type RefKind int

const (
	RefDirect RefKind = iota
	RefNamed
	RefInline
)

// Ref is a parsed egress field value.
type Ref struct {
	Kind RefKind
	Name string // RefNamed
	URL  string // RefInline: normalized
}

// ParseRef applies §7.9.2 rule 2: empty/"direct" → direct; contains "://" → URL; otherwise a node name.
func ParseRef(value string) (Ref, error) {
	v := strings.TrimSpace(value)
	switch {
	case v == "" || strings.EqualFold(v, Direct):
		return Ref{Kind: RefDirect}, nil
	case strings.Contains(v, "://"):
		n, err := NormalizeURL(v)
		if err != nil {
			return Ref{}, err
		}
		return Ref{Kind: RefInline, URL: n}, nil
	default:
		if err := ValidateName(v); err != nil {
			if looksLikeHostPort(v) {
				return Ref{}, fmt.Errorf("%q looks like an address without a scheme; write it as socks5://%s (or select a node)", v, v)
			}
			return Ref{}, err
		}
		return Ref{Kind: RefNamed, Name: v}, nil
	}
}

func looksLikeHostPort(s string) bool {
	if _, _, err := net.SplitHostPort(s); err == nil {
		return true
	}
	return strings.Count(s, ":") >= 1 && strings.Contains(s, ".")
}
