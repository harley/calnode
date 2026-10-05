package booking

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

var ErrEmailDomainBlocked = errors.New("Please use your work email address to book this meeting.")

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeBlockedDomains accepts domain names, not email addresses or wildcards.
// Empty configuration leaves an event open to all email domains.
func NormalizeBlockedDomains(domains []string) ([]string, error) {
	if len(domains) > 100 {
		return nil, errors.New("blocked_email_domains must contain at most 100 domains")
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range domains {
		domain := strings.ToLower(strings.TrimSpace(raw))
		labels := strings.Split(domain, ".")
		if len(domain) > 253 || len(labels) < 2 {
			return nil, fmt.Errorf("invalid blocked email domain: %q", raw)
		}
		for _, label := range labels {
			if !domainLabel.MatchString(label) {
				return nil, fmt.Errorf("invalid blocked email domain: %q", raw)
			}
		}
		if !seen[domain] {
			out = append(out, domain)
			seen[domain] = true
		}
	}
	return out, nil
}

// CheckEmailDomain also blocks subdomains, but never unrelated suffix matches.
func CheckEmailDomain(email string, blocked []string) error {
	if len(blocked) == 0 {
		return nil
	}
	address, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil {
		return errors.New("email must be a valid email address")
	}
	separator := strings.LastIndexByte(address.Address, '@')
	if separator < 0 {
		return errors.New("email must be a valid email address")
	}
	domain := strings.ToLower(address.Address[separator+1:])
	for _, denied := range blocked {
		if domain == denied || strings.HasSuffix(domain, "."+denied) {
			return ErrEmailDomainBlocked
		}
	}
	return nil
}
