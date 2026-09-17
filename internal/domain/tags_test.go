package domain

import (
	"strings"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "empty", in: []string{}, want: nil},
		{name: "blank token", in: []string{"  "}, wantErr: "empty values"},
		{name: "trim lowercase sort", in: []string{" Beta", "ALPHA", "gamma "}, want: []string{"alpha", "beta", "gamma"}},
		{name: "digits", in: []string{"v2", "123", "a1b2"}, want: []string{"123", "a1b2", "v2"}},
		{name: "internal hyphens", in: []string{"team-a", "prod-eu-1"}, want: []string{"prod-eu-1", "team-a"}},
		{name: "dedupe case", in: []string{"Prod", "prod", " PROD "}, want: []string{"prod"}},
		{name: "leading hyphen", in: []string{"-prod"}, wantErr: "invalid tag"},
		{name: "trailing hyphen", in: []string{"prod-"}, wantErr: "invalid tag"},
		{name: "repeated hyphen", in: []string{"prod--eu"}, wantErr: "invalid tag"},
		{name: "underscore", in: []string{"team_a"}, wantErr: "invalid tag"},
		{name: "space in token", in: []string{"team a"}, wantErr: "invalid tag"},
		{name: "other characters", in: []string{"prod!"}, wantErr: "invalid tag"},
		{name: "unicode", in: []string{"café"}, wantErr: "invalid tag"},
		{name: "unicode case folding", in: []string{"İ"}, wantErr: "invalid tag"},
		{name: "length 32 ok", in: []string{strings.Repeat("a", 32)}, want: []string{strings.Repeat("a", 32)}},
		{name: "length 33", in: []string{strings.Repeat("a", 33)}, wantErr: "exceeds 32 characters"},
		{name: "max 10", in: tenTags(), want: tenTagsSorted()},
		{name: "11 unique", in: elevenTags(), wantErr: "at most 10 tags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTags(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseTagList(t *testing.T) {
	got, err := ParseTagList(" Beta, alpha, Alpha , v2 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "beta", "v2"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	if _, err := ParseTagList("  ,  "); err == nil || !strings.Contains(err.Error(), "empty values") {
		t.Fatalf("blank list err = %v", err)
	}

	if _, err := ParseTagList("ok, foo--bar"); err == nil || !strings.Contains(err.Error(), "invalid tag") {
		t.Fatalf("repeated hyphen err = %v", err)
	}
}

func TestFormatAndDataValue(t *testing.T) {
	tags := []string{"alpha", "prod"}
	if got := FormatTags(tags); got != "alpha, prod" {
		t.Fatalf("FormatTags = %q", got)
	}
	if got := TagsDataValue(tags); got != "alpha,prod" {
		t.Fatalf("TagsDataValue = %q", got)
	}
	if FormatTags(nil) != "" || TagsDataValue(nil) != "" {
		t.Fatal("nil tags should format empty")
	}
}

func TestUniqueTagsAndHasUntagged(t *testing.T) {
	ps := []*Provider{
		{Name: "a", Tags: []string{"prod", "eu"}},
		{Name: "b", Tags: []string{"prod"}},
		{Name: "c"},
		nil,
	}
	got := UniqueTags(ps)
	want := []string{"eu", "prod"}
	if !equalStrings(got, want) {
		t.Fatalf("UniqueTags = %v, want %v", got, want)
	}
	if !HasUntagged(ps) {
		t.Fatal("want HasUntagged")
	}
	if HasUntagged([]*Provider{{Tags: []string{"prod"}}}) {
		t.Fatal("all tagged")
	}
	p := &Provider{Tags: []string{"prod"}}
	if !p.HasTag("prod") || p.HasTag("eu") {
		t.Fatal("HasTag")
	}
}

func tenTags() []string {
	return []string{"j", "i", "h", "g", "f", "e", "d", "c", "b", "a"}
}

func tenTagsSorted() []string {
	return []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
}

func elevenTags() []string {
	return append(tenTags(), "k")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
