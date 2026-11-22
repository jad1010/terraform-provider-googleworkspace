// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"testing"
)

func TestSnakeToCamel(t *testing.T) {
	input := make([]string, 3)
	input[0] = "i_am_snake_case"
	input[1] = "IAmUpperCamelCase"
	input[2] = "iAmAlreadyCamelCase"

	expected := []string{"iAmSnakeCase", "iAmUpperCamelCase", "iAmAlreadyCamelCase"}

	for it, in := range input {
		result := SnakeToCamel(in)

		if result != expected[it] {
			t.Errorf("Failed [%s]: result (%s) did not match expected (%s)", in, result, expected[it])
		}
	}
}

func TestCamelToSnake(t *testing.T) {
	input := make([]string, 3)
	input[0] = "i_am_snake_case"
	input[1] = "IAmUpperCamelCase"
	input[2] = "iAmLowerCamelCase"

	expected := []string{"i_am_snake_case", "i_am_upper_camel_case", "i_am_lower_camel_case"}

	for it, in := range input {
		result := CameltoSnake(in)

		if result != expected[it] {
			t.Errorf("Failed [%s]: result (%s) did not match expected (%s)", in, result, expected[it])
		}
	}
}

func TestIsEmail(t *testing.T) {
	type testCase struct {
		input string
		want  bool
	}

	tests := []testCase{
		{
			input: "",
			want:  false,
		},
		{
			input: "1234567890987654321",
			want:  false,
		},
		{
			input: "example.com",
			want:  false,
		},
		{
			input: "user@example.com",
			want:  true,
		},
	}

	for _, tc := range tests {
		got := isEmail(tc.input)
		if tc.want != got {
			t.Fatalf("expected: %v, got: %v", tc.want, got)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"first.last@example.com",
		"user+tag@example.com",
		"user@sub.example.co.uk",
		"o'brien@example.com",
		"user_name-1@example.com",
		"USER@EXAMPLE.COM",
		"user@localhost",
		`"a@b"@example.com`,
	}
	for _, v := range valid {
		if diags := validateEmail(v, nil); diags.HasError() {
			t.Errorf("validateEmail(%q): expected no error, got %v", v, diags)
		}
	}

	invalid := []string{
		"",
		"user",
		"example.com",
		"@example.com",
		"user@",
		"@",
		"user @example.com",
		" user@example.com",
		"user@example.com ",
		"user@example.com\n",
		"user@exa\tmple.com",
	}
	for _, v := range invalid {
		if diags := validateEmail(v, nil); !diags.HasError() {
			t.Errorf("validateEmail(%q): expected an error, got none", v)
		}
	}

	if diags := validateEmail(42, nil); !diags.HasError() {
		t.Errorf("validateEmail(42): expected an error for a non-string value, got none")
	}
}
