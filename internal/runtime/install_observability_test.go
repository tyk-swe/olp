package runtime

import "testing"

func TestInstallStatusDistinguishesPendingFailedAndRecovered(t *testing.T) {
	for _, test := range []struct {
		name                       string
		desired, installed, failed int64
		wantFailed                 bool
	}{
		{"empty", 0, 0, 0, false},
		{"pending", 4, 3, 0, false},
		{"failed", 4, 3, 4, true},
		{"newer pending", 5, 3, 4, false},
		{"recovered", 4, 4, 4, false},
		{"older failure", 5, 5, 4, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := &Manager{release: &Release{Sequence: test.installed}, failed: test.failed}
			manager.desired.Store(test.desired)
			desired, installed, failed := manager.installStatus()
			if desired != test.desired || installed != test.installed || failed != test.wantFailed {
				t.Fatalf("status = %d,%d,%t", desired, installed, failed)
			}
		})
	}
}
