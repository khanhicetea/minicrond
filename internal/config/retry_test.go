package config

import "testing"

func TestJobRetryConfig(t *testing.T) {
	defs, err := ParseImport([]byte("[[job]]\nname='flaky'\ncommand='false'\nschedule='@every 1m'\nretries=2\nretry_delay=1\n"))
	if err != nil || len(defs) != 1 || defs[0].Retries != 2 || defs[0].RetryDelay != 1 {
		t.Fatalf("parsed retry config = %+v, %v", defs, err)
	}
	defaults, err := ParseImport([]byte("[[job]]\nname='plain'\ncommand='true'\nschedule='@every 1m'\n"))
	if err != nil || defaults[0].Retries != 0 || defaults[0].RetryDelay != 5 {
		t.Fatalf("default retry config = %+v, %v", defaults, err)
	}
	for _, value := range []string{"retries=-1", "retries=1001", "retry_delay=-1", "retry_delay=86401"} {
		t.Run(value, func(t *testing.T) {
			input := "[[job]]\nname='bad'\ncommand='false'\nschedule='@every 1m'\n" + value + "\n"
			if _, err := ParseImport([]byte(input)); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
	for _, value := range []string{"retries=1", "retry_delay=5"} {
		t.Run("worker-"+value, func(t *testing.T) {
			input := "[[worker]]\nname='bad'\ncommand='false'\n" + value + "\n"
			if _, err := ParseImport([]byte(input)); err == nil {
				t.Fatal("worker retries must be rejected")
			}
		})
	}
}
