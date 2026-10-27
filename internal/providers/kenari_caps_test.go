package providers

import "testing"

// Kenari (kenari.id) is an image-only OpenAI-compatible aggregator: all six of
// its ids accept a photo as edit input (vision) and return an image. The rows
// are provider-scoped so the shared gpt-image-* ids keep the global caps the
// other providers serving them rely on.
func TestGetCapabilitiesForModel_KenariImageModels(t *testing.T) {
	tests := []struct {
		model string
	}{
		{"agnes-image-2-0-flash:free"},
		{"agnes-image-2-1-flash:free"},
		{"agnes-image-2-5-flash:free"},
		{"gpt-image-2"},
		{"gpt-image-2-5-flare"},
		{"gpt-image-2-5-sunburst"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			caps := GetCapabilitiesForModel("kenari", tt.model)
			if !caps.Vision {
				t.Errorf("Vision = false, want true")
			}
			if !caps.ImageOutput {
				t.Errorf("ImageOutput = false, want true")
			}
		})
	}
}

// The provider-scoped kenari rows must not leak into other providers: the
// global gpt-image-2 caps (pattern *gpt-image*) carry ImageOutput but no Vision.
func TestGetCapabilitiesForModel_GlobalGPTImageCapsUnchanged(t *testing.T) {
	caps := GetCapabilitiesForModel("openai", "gpt-image-2")
	if !caps.ImageOutput {
		t.Error("ImageOutput = false, want true (global pattern cap)")
	}
	if caps.Vision {
		t.Error("Vision = true, want false (kenari's Vision row must stay provider-scoped)")
	}
}
