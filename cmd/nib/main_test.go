package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	for _, tt := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"no build info", nil, "v0.2.0"},
		{"empty build info", &debug.BuildInfo{}, "v0.2.0"},
		{"development build", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "v0.2.0"},
		{"module release", &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}}, "v0.2.0"},
		{
			"untagged checkout",
			&debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20261015223248-dbfee224be25"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "dbfee224be253a4a0bed09bea0b25538bb33cfdb"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			"v0.2.0 dbfee224",
		},
		{
			"modified checkout",
			&debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20261015223248-dbfee224be25+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.modified", Value: "true"},
					{Key: "vcs.revision", Value: "dbfee224be253a4a0bed09bea0b25538bb33cfdb"},
				},
			},
			"v0.2.0+dirty dbfee224",
		},
		{
			"short revision",
			&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}},
			"v0.2.0",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.info); got != tt.want {
				t.Fatalf("buildVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
