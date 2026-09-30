package fault

import (
	"errors"
	"strings"
	"testing"
)

func TestCallPreservesErrorsAndRecoveryStack(t *testing.T) {
	cause := errors.New("operation failed")
	if got := Call(func() error { return cause }); got != cause {
		t.Fatalf("ordinary error changed: %v", got)
	}
	for _, value := range []any{cause, "broken invariant", nil} {
		err := Call(func() error { panic(value) })
		p, ok := errors.AsType[*PanicError](err)
		if !ok || len(p.Stack) == 0 || !strings.Contains(string(p.Stack), "TestCallPreservesErrorsAndRecoveryStack") {
			t.Fatalf("missing panic diagnostics: %#v", err)
		}
		if value == cause && !errors.Is(err, cause) {
			t.Fatalf("panic error chain lost: %v", err)
		}
	}
	if err := Call(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
