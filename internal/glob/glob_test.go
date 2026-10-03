package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		// Exact and case folding.
		{"pan", "pan", true},
		{"PAN", "pan", true},
		{"pan", "pan_number", false},

		// Star matching anywhere. '*' spans dots, so it is not path-aware.
		{"*.pan", "pan", false},
		{"*.pan", "employees.pan", true},
		{"*.pan", "public.employees.pan", true},
		{"*.pan", "pancake", false},
		{"*password*", "user_password_hash", true},
		{"*password*", "public.users.password_hash", true},
		{"*password*", "email", false},

		// Star crossing dots is intentional: "*" is not path-aware.
		{"public.*", "public.employees.pan", true},

		// Question mark.
		{"pa?", "pan", true},
		{"pa?", "pa", false},

		// Trailing and leading stars.
		{"*", "anything.at.all", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXcYYb", false},

		// Anchoring.
		{"pan*", "pan_number", true},
		{"pan*", "company_pan", false},

		// Adjacent stars.
		{"**pan", "pan", true},
		{"a**b", "ab", true},

		// Backtracking stress: the star must give up characters again.
		{"*abc", "abcabc", true},
		{"a*abc", "aaabc", true},
		{"*a*a*a*b", "aaaab", true},
		{"*a*a*a*b", "aaaac", false},
	}

	for _, tc := range cases {
		if got := Match(tc.pattern, tc.s); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMatchName(t *testing.T) {
	// A hand-written "*.pan" is understood as "a column called pan anywhere".
	panPatterns := []string{"*.pan"}
	if !MatchName(panPatterns, "pan", "cards.pan") {
		t.Error("*.pan should match a qualified column")
	}
	if !MatchName(panPatterns, "pan", "pan") {
		t.Error("*.pan should also match a bare column name")
	}
	if MatchName(panPatterns, "pancake", "t.pancake") {
		t.Error("*.pan must not match pancake")
	}

	// A pattern with no leading star is matched literally.
	if !MatchName([]string{"password"}, "password", "users.password") {
		t.Error("literal pattern should match")
	}
	if MatchName([]string{"password"}, "password_hash", "users.password_hash") {
		t.Error("literal pattern must not match a longer name")
	}

	if !MatchName([]string{"*password*"}, "password_hash", "users.password_hash") {
		t.Error("substring pattern should match")
	}
}

func TestMatchAny(t *testing.T) {
	patterns := []string{"*.pan", "*password*"}

	if !MatchAny(patterns, "users.password_hash") {
		t.Error("expected password_hash to match")
	}
	if !MatchAny(patterns, "kyc.pan") {
		t.Error("expected pan to match")
	}
	if MatchAny(patterns, "users.email") {
		t.Error("email should not match")
	}
	if MatchAny(nil, "anything") {
		t.Error("nil patterns should match nothing")
	}
}
