package wechat

import "testing"

func TestParseLegacyDecryptFlag_Truthy(t *testing.T) {
	for _, in := range []string{"1", "true", "TRUE", "True", "yes", "YES", "on", "  yes  ", "1\n"} {
		if !parseLegacyDecryptFlag(in) {
			t.Errorf("expected %q parsed truthy", in)
		}
	}
}

func TestParseLegacyDecryptFlag_Falsy(t *testing.T) {
	for _, in := range []string{"", "0", "false", "no", "off", "anything-else", "yes-but-not", " "} {
		if parseLegacyDecryptFlag(in) {
			t.Errorf("expected %q parsed falsy", in)
		}
	}
}

// TestIsLegacyDecryptEnabled_RespectsEnv：通过 t.Setenv 验证环境变量驱动。
func TestIsLegacyDecryptEnabled_RespectsEnv(t *testing.T) {
	t.Setenv(LegacyDecryptEnv, "")
	if IsLegacyDecryptEnabled() {
		t.Errorf("empty env should be false")
	}
	t.Setenv(LegacyDecryptEnv, "1")
	if !IsLegacyDecryptEnabled() {
		t.Errorf("env=1 should be true")
	}
	t.Setenv(LegacyDecryptEnv, "off")
	if IsLegacyDecryptEnabled() {
		t.Errorf("env=off should be false")
	}
}
