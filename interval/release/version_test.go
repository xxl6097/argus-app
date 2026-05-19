package release

import "testing"

func TestHasUpdate(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.13", "v0.1.14", true},
		{"v0.1.13", "v0.1.13", false},
		{"v0.1.13", "v0.1.12", false},
		{"v0.2.0", "v0.10.0", true},
		{"0.1.13", "v0.1.14", true},
		{"v1.0.0", "v0.99.99", false},
		{"", "v1.0.0", false},
		{"dev", "v1.0.0", false},
		{"v1.0.0", "", false},
		{"v0.1.13-rc1", "v0.1.13", true},     // pre-release upgrades to release
		{"v0.1.13-test", "v0.1.13", true},    // -test 也算 pre-release
		{"v0.1.13", "v0.1.13-rc1", false},    // release 不该被 rc 拉回去
		{"v0.1.13-rc1", "v0.1.14", true},     // 同时跨小版本 + 转正
	}
	for _, tt := range tests {
		got := HasUpdate(tt.current, tt.latest)
		if got != tt.want {
			t.Errorf("HasUpdate(%q,%q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}
