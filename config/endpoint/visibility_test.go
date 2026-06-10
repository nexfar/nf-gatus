package endpoint

import (
	"errors"
	"testing"
)

func TestVisibility_ValidateAndSetDefault(t *testing.T) {
	scenarios := []struct {
		input         Visibility
		expected      Visibility
		expectedError error
	}{
		{input: "", expected: VisibilityPrivate},
		{input: VisibilityPrivate, expected: VisibilityPrivate},
		{input: VisibilityPublic, expected: VisibilityPublic},
		{input: "Public", expectedError: ErrInvalidVisibility},
		{input: "hidden", expectedError: ErrInvalidVisibility},
	}
	for _, scenario := range scenarios {
		t.Run(string(scenario.input), func(t *testing.T) {
			visibility := scenario.input
			err := visibility.ValidateAndSetDefault()
			if !errors.Is(err, scenario.expectedError) {
				t.Errorf("expected error %v, got %v", scenario.expectedError, err)
			}
			if err == nil && visibility != scenario.expected {
				t.Errorf("expected visibility %s, got %s", scenario.expected, visibility)
			}
		})
	}
}

func TestVisibility_IsPublic(t *testing.T) {
	if VisibilityPrivate.IsPublic() {
		t.Error("private visibility should not be public")
	}
	if (Visibility("")).IsPublic() {
		t.Error("unset visibility should not be public")
	}
	if !VisibilityPublic.IsPublic() {
		t.Error("public visibility should be public")
	}
}

func TestEndpoint_ValidateAndSetDefaultsWithInvalidVisibility(t *testing.T) {
	e := &Endpoint{
		Name:       "name",
		URL:        "https://example.org",
		Visibility: "everyone",
		Conditions: []Condition{Condition("[STATUS] == 200")},
	}
	if err := e.ValidateAndSetDefaults(); !errors.Is(err, ErrInvalidVisibility) {
		t.Errorf("expected ErrInvalidVisibility, got %v", err)
	}
}
