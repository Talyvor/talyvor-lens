package phonesignin

import (
	"errors"
	"testing"
)

func TestCheckPasscodeShape(t *testing.T) {
	const phone = "+447700958204"
	for _, weak := range []string{"", "12345", "1234567", "12a456", "000000", "999999", "123456", "456789", "012345", "654321", "543210", "958204"} {
		var w *WeakPasscodeError
		if err := CheckPasscodeShape(weak, phone); !errors.As(err, &w) || w.Reason == "" {
			t.Errorf("%q: %v, want refused with a reason", weak, err)
		}
	}
	for _, good := range []string{"583920", "121212", "123465", "890123", "958205"} {
		if err := CheckPasscodeShape(good, phone); err != nil {
			t.Errorf("%q: %v, want allowed", good, err)
		}
	}
}
