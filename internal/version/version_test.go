package version_test

import (
	"testing"

	"github.com/mchmarny/aicrme/internal/version"
)

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "dev default", version: "dev", commit: "none", want: "dev (none)"},
		{name: "release", version: "v0.1.0", commit: "abc1234", want: "v0.1.0 (abc1234)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version.Version = tc.version
			version.Commit = tc.commit
			if got := version.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
