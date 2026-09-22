package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	MaxTagLength    = 32
	MaxProviderTags = 10
)

// Canonical tag: lowercase ASCII letters, digits, and internal hyphens only.
var tagPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ParseTagList splits a comma-separated form value, then normalizes.
func ParseTagList(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return NormalizeTags(strings.Split(s, ","))
}

// NormalizeTags trims, lowercases ASCII letters, rejects invalid syntax,
// deduplicates, and sorts.
func NormalizeTags(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		t := strings.TrimSpace(r)
		if t == "" {
			return nil, fmt.Errorf("tags cannot contain empty values")
		}
		t = lowercaseASCII(t)
		if err := validateTag(t); err != nil {
			return nil, err
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > MaxProviderTags {
		return nil, fmt.Errorf("at most %d tags per provider", MaxProviderTags)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func lowercaseASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func validateTag(t string) error {
	if len(t) > MaxTagLength {
		return fmt.Errorf("tag %q exceeds %d characters", t, MaxTagLength)
	}
	if !tagPattern.MatchString(t) {
		return fmt.Errorf("invalid tag %q: use lowercase letters, digits, and internal hyphens", t)
	}
	return nil
}

// FormatTags joins canonical tags for a comma-separated form field.
func FormatTags(tags []string) string {
	return strings.Join(tags, ", ")
}

// TagTooltip returns the native tooltip for a tagged provider. Empty tags
// return "" so callers omit the title attribute.
func TagTooltip(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return "Tags: " + FormatTags(tags)
}

// TagsDataValue joins canonical tags for a data-tags attribute (no spaces).
func TagsDataValue(tags []string) string {
	return strings.Join(tags, ",")
}

// UniqueTags returns the sorted union of tags across providers.
func UniqueTags(providers []*Provider) []string {
	seen := map[string]struct{}{}
	for _, p := range providers {
		if p == nil {
			continue
		}
		for _, t := range p.Tags {
			if t == "" {
				continue
			}
			seen[t] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// HasUntagged reports whether any provider has no tags.
func HasUntagged(providers []*Provider) bool {
	for _, p := range providers {
		if p != nil && len(p.Tags) == 0 {
			return true
		}
	}
	return false
}

// HasTag reports whether p carries the exact canonical tag.
func (p *Provider) HasTag(tag string) bool {
	if p == nil {
		return false
	}
	for _, t := range p.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
