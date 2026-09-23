package localdolt

import (
	"errors"
	"testing"
)

func TestCheckMatchesDisabled(t *testing.T) {
	err := Check("dolt sql-server auto-start")
	if Disabled() {
		if !errors.Is(err, ErrDisabled) {
			t.Fatalf("Check() = %v, want wrapped ErrDisabled in a remote-only build", err)
		}
		if got, want := err.Error(), "dolt sql-server auto-start: "+ErrDisabled.Error(); got != want {
			t.Fatalf("Check() = %q, want %q", got, want)
		}
		return
	}
	if err != nil {
		t.Fatalf("Check() = %v, want nil in a default build", err)
	}
}
