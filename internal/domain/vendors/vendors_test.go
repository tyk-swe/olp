package vendors

import "testing"

func TestSoleSpeakerNamesNoVendorForADialectAnotherVendorSpeaks(t *testing.T) {
	t.Parallel()
	for _, c := range All() {
		for _, dialect := range c.Dialects {
			if sole, named := SoleSpeaker(dialect); named && sole != c.ID {
				t.Errorf("SoleSpeaker(%q) = %q, and %s speaks it too", dialect, sole, c.ID)
			}
		}
	}
}
