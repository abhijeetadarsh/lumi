package googletts

import "testing"

func TestLanguageOf(t *testing.T) {
	tests := []struct {
		voice string
		want  string
		ok    bool
	}{
		{"en-US-Standard-B", "en-US", true},
		{"en-US-Chirp3-HD-Kore", "en-US", true},
		{"en-US-Neural2-F", "en-US", true},
		{"en-GB-Neural2-A", "en-GB", true},
		{"cmn-CN-Wavenet-A", "cmn-CN", true},
		{"en-US", "en-US", true},
		{"es-ES-Neural2-A", "es-ES", true},
		// a bare language code has no region to disambiguate
		{"en", "", false},
		// script subtags continue the tag
		{"zh-CN", "zh-CN", true},
	}
	for _, tc := range tests {
		got, ok := languageOf(tc.voice)
		if got != tc.want || ok != tc.ok {
			t.Errorf("languageOf(%q) = (%q, %v), want (%q, %v)", tc.voice, got, ok, tc.want, tc.ok)
		}
	}
}

// A named voice must be sent with its region intact: Google rejects a bare "en"
// when the voice declares "en-US".
func TestNamedVoiceSendsFullLanguageCode(t *testing.T) {
	tests := []struct {
		voice string
		want  string
	}{
		{"en-US-Chirp3-HD-Kore", "en-US"},
		{"en-US-Wavenet-F", "en-US"},
		{"cmn-CN-Wavenet-A", "cmn-CN"},
	}
	for _, tc := range tests {
		p := synthRequest{}
		p.Input.Text = "hi"
		lang, ok := languageOf(tc.voice)
		if !ok {
			t.Fatalf("languageOf(%q) failed", tc.voice)
		}
		p.Voice.Name, p.Voice.LanguageCode = tc.voice, lang
		if p.Voice.LanguageCode != tc.want {
			t.Errorf("voice %q: languageCode = %q, want %q", tc.voice, p.Voice.LanguageCode, tc.want)
		}
		if len(p.Voice.LanguageCode) < 4 {
			t.Errorf("voice %q: languageCode %q looks truncated to a bare language", tc.voice, p.Voice.LanguageCode)
		}
	}
}

// isAlpha only reports the character class; length is checked by the caller.
func TestIsAlphaIsDigits(t *testing.T) {
	if !isAlpha("US") || !isAlpha("gb") || isAlpha("12") || isAlpha("") {
		t.Error("isAlpha is wrong")
	}
	if !isDigits("419") || isDigits("US") || isDigits("") {
		t.Error("isDigits is wrong")
	}
}
