package memory

import (
	"os"
	"testing"
)

func TestPIIMasker_Builtin(t *testing.T) {
	masker := NewPIIMasker()

	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "Here is my AWS key: AKIAIOSFODNN7EXAMPLE",
			expected: "Here is my AWS key: [REDACTED:AWS_KEY]",
		},
		{
			input:    "Email me at user@example.com",
			expected: "Email me at [REDACTED:EMAIL]",
		},
		{
			input:    "The password is password='secret12345'",
			expected: "The password is [REDACTED:SECRET]",
		},
	}

	for _, tc := range tests {
		actual := masker.Mask(tc.input)
		if actual != tc.expected {
			t.Errorf("expected %q, got %q", tc.expected, actual)
		}
	}
}

func TestPIIMasker_CustomEnv(t *testing.T) {
	// Set custom env variable for testing
	os.Setenv("AGENT_CUSTOM_PII_REGEX", "MY_TOKEN=sk-corp-[A-Z0-9]+,RBAC_ROLE=role-[a-z]+")
	defer os.Unsetenv("AGENT_CUSTOM_PII_REGEX")

	masker := NewPIIMasker()

	input1 := "My token is sk-corp-1234ABCD"
	expected1 := "My token is [REDACTED:MY_TOKEN]"
	actual1 := masker.Mask(input1)
	if actual1 != expected1 {
		t.Errorf("expected %q, got %q", expected1, actual1)
	}

	input2 := "Assign role-admin to this user"
	expected2 := "Assign [REDACTED:RBAC_ROLE] to this user"
	actual2 := masker.Mask(input2)
	if actual2 != expected2 {
		t.Errorf("expected %q, got %q", expected2, actual2)
	}
}
