package config

import (
	"os"
	"testing"
)

func TestDebugLevelSet(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 1, false},
		{"true", 1, false},
		{"TRUE", 1, false},
		{"yes", 1, false},
		{"on", 1, false},
		{"false", 0, false},
		{"no", 0, false},
		{"off", 0, false},
		{"1", 1, false},
		{"2", 2, false},
		{"3", 3, false},
		{"0", 0, false},
		{"garbage", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var d debugLevel
			err := d.Set(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q) = %v, want nil", tc.in, err)
			}
			if int(d) != tc.want {
				t.Errorf("level = %d, want %d", int(d), tc.want)
			}
		})
	}
}

func TestDebugLevelString(t *testing.T) {
	d := debugLevel(2)
	if got := d.String(); got != "2" {
		t.Errorf("String() = %q, want \"2\"", got)
	}
}

func TestDebugLevelIsBoolFlag(t *testing.T) {
	var d debugLevel
	if !d.IsBoolFlag() {
		t.Error("IsBoolFlag() = false, want true (bare -debug must be accepted)")
	}
}

func TestEnvDebugLevel(t *testing.T) {
	cases := []struct {
		name string
		val  string
		set  bool // whether to set the env var at all
		want int
	}{
		{"unset", "", false, 0},
		{"empty", "", true, 0},
		{"true", "true", true, 1},
		{"TRUE", "TRUE", true, 1},
		{"yes", "yes", true, 1},
		{"on", "on", true, 1},
		{"false", "false", true, 0},
		{"no", "no", true, 0},
		{"off", "off", true, 0},
		{"numeric 2", "2", true, 2},
		{"numeric 3", "3", true, 3},
		{"garbage", "garbage", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("AIROUTER_DEBUG", tc.val)
			} else {
				t.Setenv("AIROUTER_DEBUG", "")
			}
			// t.Setenv sets the var even to ""; to truly unset, clear it.
			if !tc.set {
				clearEnvVar(t, "AIROUTER_DEBUG")
			}
			if got := envDebugLevel(); got != tc.want {
				t.Errorf("envDebugLevel() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveSecret(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		c := Config{Secret: "my-real-secret"}
		s, isDev := c.EffectiveSecret()
		if s != "my-real-secret" {
			t.Errorf("secret = %q, want my-real-secret", s)
		}
		if isDev {
			t.Error("isDev = true, want false for a configured secret")
		}
	})
	t.Run("empty falls back to dev", func(t *testing.T) {
		c := Config{Secret: ""}
		s, isDev := c.EffectiveSecret()
		if s != devSecret {
			t.Errorf("secret = %q, want devSecret", s)
		}
		if !isDev {
			t.Error("isDev = false, want true for the insecure fallback")
		}
	})
}

func TestEnv(t *testing.T) {
	t.Run("present returns value", func(t *testing.T) {
		t.Setenv("AIROUTER_TEST_VAR", "custom")
		if got := env("AIROUTER_TEST_VAR", "default"); got != "custom" {
			t.Errorf("env = %q, want custom", got)
		}
	})
	t.Run("absent returns default", func(t *testing.T) {
		clearEnvVar(t, "AIROUTER_TEST_VAR")
		if got := env("AIROUTER_TEST_VAR", "default"); got != "default" {
			t.Errorf("env = %q, want default", got)
		}
	})
	t.Run("empty returns default", func(t *testing.T) {
		t.Setenv("AIROUTER_TEST_VAR", "")
		if got := env("AIROUTER_TEST_VAR", "default"); got != "default" {
			t.Errorf("env = %q, want default (empty env is treated as absent)", got)
		}
	})
}

// clearEnvVar unsets a var for the test and restores it afterward.
func clearEnvVar(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %q: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}
