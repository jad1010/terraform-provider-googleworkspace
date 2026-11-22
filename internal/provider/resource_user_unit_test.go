package googleworkspace

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// userRawConfig returns a raw configuration value for googleworkspace_user.
// All attributes are null except the attributes in attrs.
func userRawConfig(attrs map[string]cty.Value) cty.Value {
	ty := resourceUser().CoreConfigSchema().ImpliedType()

	vals := make(map[string]cty.Value, len(ty.AttributeTypes()))
	for name, attrTy := range ty.AttributeTypes() {
		vals[name] = cty.NullVal(attrTy)
	}

	for name, v := range attrs {
		vals[name] = v
	}

	return cty.ObjectVal(vals)
}

// userRawName returns a raw configuration value for the name block.
func userRawName(familyName, givenName string) cty.Value {
	listTy := resourceUser().CoreConfigSchema().ImpliedType().AttributeType("name")
	objTy := listTy.ElementType()

	vals := make(map[string]cty.Value, len(objTy.AttributeTypes()))
	for name, attrTy := range objTy.AttributeTypes() {
		vals[name] = cty.NullVal(attrTy)
	}
	vals["family_name"] = cty.StringVal(familyName)
	vals["given_name"] = cty.StringVal(givenName)

	return cty.ListVal([]cty.Value{cty.ObjectVal(vals)})
}

func TestUserPasswordToSend(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		passwordChanged          bool
		password                 string
		passwordWOVersionChanged bool
		passwordWO               string
		want                     string
		wantSend                 bool
	}{
		"nothing changed": {
			password: "old-password",
		},
		"password changed": {
			passwordChanged: true,
			password:        "new-password",
			want:            "new-password",
			wantSend:        true,
		},
		"password removed sends empty value as before": {
			passwordChanged: true,
			wantSend:        true,
		},
		"password_wo_version changed": {
			passwordWOVersionChanged: true,
			passwordWO:               "write-only-password",
			want:                     "write-only-password",
			wantSend:                 true,
		},
		"password_wo changed without version change is ignored": {
			passwordWO: "write-only-password",
		},
		"switch from password to password_wo sends password_wo": {
			passwordChanged:          true,
			passwordWOVersionChanged: true,
			passwordWO:               "write-only-password",
			want:                     "write-only-password",
			wantSend:                 true,
		},
		"switch from password_wo to password sends password": {
			passwordChanged:          true,
			password:                 "new-password",
			passwordWOVersionChanged: true,
			want:                     "new-password",
			wantSend:                 true,
		},
		"password_wo and version removed sends nothing": {
			passwordWOVersionChanged: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, gotSend := userPasswordToSend(test.passwordChanged, test.password, test.passwordWOVersionChanged, test.passwordWO)
			if got != test.want || gotSend != test.wantSend {
				t.Errorf("userPasswordToSend() = (%q, %t), want (%q, %t)", got, gotSend, test.want, test.wantSend)
			}
		})
	}
}

func TestUserPasswordConfigured(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rawConfig cty.Value
		want      bool
	}{
		"neither set": {
			rawConfig: userRawConfig(nil),
			want:      false,
		},
		"password set": {
			rawConfig: userRawConfig(map[string]cty.Value{"password": cty.StringVal("password1")}),
			want:      true,
		},
		"password_wo set": {
			rawConfig: userRawConfig(map[string]cty.Value{"password_wo": cty.StringVal("password1")}),
			want:      true,
		},
		"password_wo unknown": {
			rawConfig: userRawConfig(map[string]cty.Value{"password_wo": cty.UnknownVal(cty.String)}),
			want:      true,
		},
		"only password_wo_version set": {
			rawConfig: userRawConfig(map[string]cty.Value{"password_wo_version": cty.NumberIntVal(1)}),
			want:      false,
		},
		"null config": {
			rawConfig: cty.NullVal(resourceUser().CoreConfigSchema().ImpliedType()),
			want:      true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := userPasswordConfigured(test.rawConfig); got != test.want {
				t.Errorf("userPasswordConfigured() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestResourceUserValidatePasswordArguments(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config  map[string]interface{}
		wantErr string
	}{
		"password": {
			config: map[string]interface{}{"password": "password1"},
		},
		"password_wo with version": {
			config: map[string]interface{}{"password_wo": "password1", "password_wo_version": 1},
		},
		"neither, as after an import": {
			config: map[string]interface{}{},
		},
		"password and password_wo": {
			config:  map[string]interface{}{"password": "password1", "password_wo": "password1", "password_wo_version": 1},
			wantErr: "conflicts with",
		},
		"password_wo without version": {
			config:  map[string]interface{}{"password_wo": "password1"},
			wantErr: "must be specified",
		},
		"version without password_wo": {
			config:  map[string]interface{}{"password": "password1", "password_wo_version": 1},
			wantErr: "must be specified",
		},
		"password_wo_version 0": {
			config:  map[string]interface{}{"password_wo": "password1", "password_wo_version": 0},
			wantErr: "expected password_wo_version to be at least (1)",
		},
		"password_wo too short": {
			config:  map[string]interface{}{"password_wo": "short", "password_wo_version": 1},
			wantErr: "expected length",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			raw := map[string]interface{}{
				"primary_email": "user@example.com",
				"name": []interface{}{
					map[string]interface{}{"family_name": "Scott", "given_name": "Michael"},
				},
			}
			for k, v := range test.config {
				raw[k] = v
			}

			diags := resourceUser().Validate(terraform.NewResourceConfigRaw(raw))

			var errs []string
			for _, d := range diags {
				errs = append(errs, d.Summary+": "+d.Detail)
			}
			got := strings.Join(errs, "; ")

			if test.wantErr == "" && diags.HasError() {
				t.Fatalf("unexpected error: %s", got)
			}
			if test.wantErr != "" && !strings.Contains(got, test.wantErr) {
				t.Fatalf("expected error containing %q, got %q", test.wantErr, got)
			}
		})
	}
}

func TestResourceUserCustomizeDiffPassword(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		id        string
		rawConfig cty.Value
		wantErr   bool
	}{
		"create with password_wo": {
			rawConfig: userRawConfig(map[string]cty.Value{
				"primary_email":       cty.StringVal("user@example.com"),
				"password_wo":         cty.StringVal("password1"),
				"password_wo_version": cty.NumberIntVal(1),
			}),
		},
		"create with password": {
			rawConfig: userRawConfig(map[string]cty.Value{
				"primary_email": cty.StringVal("user@example.com"),
				"password":      cty.StringVal("password1"),
			}),
		},
		"create without password": {
			rawConfig: userRawConfig(map[string]cty.Value{
				"primary_email": cty.StringVal("user@example.com"),
			}),
			wantErr: true,
		},
		"update without password": {
			id: "123456789012345678901",
			rawConfig: userRawConfig(map[string]cty.Value{
				"primary_email": cty.StringVal("user@example.com"),
			}),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			r := resourceUser()
			state := &terraform.InstanceState{
				ID:         test.id,
				Attributes: map[string]string{"primary_email": "user@example.com"},
				RawConfig:  test.rawConfig,
			}
			if test.id != "" {
				state.Attributes["id"] = test.id
			}
			config := terraform.NewResourceConfigShimmed(test.rawConfig, r.CoreConfigSchema())

			_, err := r.Diff(context.Background(), state, config, nil)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "Password is required") {
					t.Fatalf("expected password required error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
		})
	}
}

func TestWriteOnlySupported(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		versionOutput string
		want          bool
		wantErr       bool
	}{
		"terraform 1.5":            {versionOutput: "Terraform v1.5.7\non linux_amd64\n", want: false},
		"terraform 1.10":           {versionOutput: "Terraform v1.10.5\non linux_amd64\n", want: false},
		"terraform 1.11":           {versionOutput: "Terraform v1.11.0\non linux_amd64\n", want: true},
		"terraform 1.12":           {versionOutput: "Terraform v1.12.2\non linux_amd64\n", want: true},
		"terraform 2.0":            {versionOutput: "Terraform v2.0.0\n", want: true},
		"opentofu 1.8":             {versionOutput: "OpenTofu v1.8.9\non linux_amd64\n", want: false},
		"opentofu 1.11":            {versionOutput: "OpenTofu v1.11.1\non linux_amd64\n", want: true},
		"TF_ACC_TERRAFORM_VERSION": {versionOutput: "1.9.8", want: false},
		"prerelease":               {versionOutput: "Terraform v1.11.0-beta1\n", want: true},
		"no version":               {versionOutput: "Terraform\n", wantErr: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := writeOnlySupported(test.versionOutput)
			if (err != nil) != test.wantErr {
				t.Fatalf("writeOnlySupported() error = %v, wantErr %t", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("writeOnlySupported() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestResourceUserPreferPasswordWOWarning(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rawConfig         cty.Value
		writeOnlyAllowed  bool
		wantWarningOnPath string
	}{
		"password on a client with write-only support": {
			rawConfig:         userRawConfig(map[string]cty.Value{"password": cty.StringVal("password1")}),
			writeOnlyAllowed:  true,
			wantWarningOnPath: "password",
		},
		"password on a client without write-only support": {
			rawConfig: userRawConfig(map[string]cty.Value{"password": cty.StringVal("password1")}),
		},
		"password_wo on a client with write-only support": {
			rawConfig: userRawConfig(map[string]cty.Value{
				"password_wo":         cty.StringVal("password1"),
				"password_wo_version": cty.NumberIntVal(1),
			}),
			writeOnlyAllowed: true,
		},
		"no password on a client with write-only support": {
			rawConfig:        userRawConfig(nil),
			writeOnlyAllowed: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			for _, f := range resourceUser().ValidateRawResourceConfigFuncs {
				resp := &schema.ValidateResourceConfigFuncResponse{}
				f(context.Background(), schema.ValidateResourceConfigFuncRequest{
					WriteOnlyAttributesAllowed: test.writeOnlyAllowed,
					RawConfig:                  test.rawConfig,
				}, resp)
				diags = append(diags, resp.Diagnostics...)
			}

			if test.wantWarningOnPath == "" {
				if len(diags) != 0 {
					t.Fatalf("expected no diagnostics, got %+v", diags)
				}
				return
			}

			if len(diags) != 1 || diags[0].Severity != diag.Warning ||
				!diags[0].AttributePath.Equals(cty.GetAttrPath(test.wantWarningOnPath)) ||
				!strings.Contains(diags[0].Detail, "password_wo") {
				t.Fatalf("expected one warning on %s that names password_wo, got %+v", test.wantWarningOnPath, diags)
			}
		})
	}
}

func TestDataSourceUserSchema(t *testing.T) {
	t.Parallel()

	schemas := map[string]map[string]*schema.Schema{
		"googleworkspace_user":  dataSourceUser().Schema,
		"googleworkspace_users": dataSourceUsers().Schema["users"].Elem.(*schema.Resource).Schema,
	}

	for name, s := range schemas {
		for _, key := range []string{"password_wo", "password_wo_version"} {
			if _, ok := s[key]; ok {
				t.Errorf("%s: unexpected attribute %s", name, key)
			}
		}

		for _, key := range []string{"password", "hash_function"} {
			if strings.Contains(s[key].Description, "password_wo") {
				t.Errorf("%s: %s description mentions password_wo: %q", name, key, s[key].Description)
			}
		}
	}

	// Check the schema that Terraform receives. The SDK turns a computed-only
	// list of nested resources into one list(object) attribute and drops the
	// Sensitive flags of the nested attributes.
	single := dataSourceUser().CoreConfigSchema()
	for _, key := range []string{"recovery_email", "recovery_phone"} {
		attr, ok := single.Attributes[key]
		if !ok || !attr.Sensitive {
			t.Errorf("googleworkspace_user: %s is not sensitive in the core schema", key)
		}
	}

	plural := dataSourceUsers()
	if users, ok := plural.CoreConfigSchema().Attributes["users"]; !ok || users.Sensitive {
		t.Errorf("googleworkspace_users: users must be a non-sensitive attribute")
	}

	// A nested Sensitive flag has no effect in the plural data source, so it
	// must not suggest that the values are hidden.
	for key, s := range plural.Schema["users"].Elem.(*schema.Resource).Schema {
		if s.Sensitive {
			t.Errorf("googleworkspace_users: nested attribute %s sets Sensitive, which Terraform ignores", key)
		}
	}
}

// TestResourceUserUpdatePasswordWiring builds the ResourceData for an update
// through the SDK Diff and Apply of the real resource schema. A stub replaces
// the update function, so the test makes no API calls. The stub records which
// password resourceUserUpdate would send.
func TestResourceUserUpdatePasswordWiring(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state           map[string]string
		config          map[string]cty.Value
		wantValue       string
		wantSend        bool
		wantValidateErr string
	}{
		"password_wo_version 0 is rejected": {
			state: map[string]string{"password": "old-password"},
			config: map[string]cty.Value{
				"password_wo":         cty.StringVal("new-write-only"),
				"password_wo_version": cty.NumberIntVal(0),
			},
			wantValidateErr: "to be at least (1)",
		},
		"password_wo_version bump sends password_wo": {
			state: map[string]string{"password_wo_version": "1"},
			config: map[string]cty.Value{
				"password_wo":         cty.StringVal("new-write-only"),
				"password_wo_version": cty.NumberIntVal(2),
			},
			wantValue: "new-write-only",
			wantSend:  true,
		},
		"new password_wo without version bump sends nothing": {
			state: map[string]string{"password_wo_version": "1"},
			config: map[string]cty.Value{
				"password_wo":         cty.StringVal("new-write-only"),
				"password_wo_version": cty.NumberIntVal(1),
			},
		},
		"password to password_wo sends password_wo": {
			state: map[string]string{"password": "old-password"},
			config: map[string]cty.Value{
				"password_wo":         cty.StringVal("new-write-only"),
				"password_wo_version": cty.NumberIntVal(1),
			},
			wantValue: "new-write-only",
			wantSend:  true,
		},
		"password_wo to password sends password": {
			state:     map[string]string{"password_wo_version": "1"},
			config:    map[string]cty.Value{"password": cty.StringVal("new-password")},
			wantValue: "new-password",
			wantSend:  true,
		},
		"imported user with password_wo sends password_wo": {
			state: map[string]string{},
			config: map[string]cty.Value{
				"password_wo":         cty.StringVal("new-write-only"),
				"password_wo_version": cty.NumberIntVal(1),
			},
			wantValue: "new-write-only",
			wantSend:  true,
		},
		"password removed sends empty password as before": {
			state:    map[string]string{"password": "old-password"},
			config:   map[string]cty.Value{},
			wantSend: true,
		},
		"password unchanged sends nothing": {
			state:  map[string]string{"password": "old-password"},
			config: map[string]cty.Value{"password": cty.StringVal("old-password")},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			const id = "123456789012345678901"

			var gotValue string
			var gotSend, called bool

			r := resourceUser()
			r.UpdateContext = func(_ context.Context, d *schema.ResourceData, _ interface{}) diag.Diagnostics {
				var diags diag.Diagnostics
				called = true
				gotValue, gotSend, diags = userPasswordForUpdate(d)
				return diags
			}

			configAttrs := map[string]cty.Value{
				"primary_email": cty.StringVal("user@example.com"),
				"name":          userRawName("Scott", "Michael"),
			}
			for k, v := range test.config {
				configAttrs[k] = v
			}
			rawConfig := userRawConfig(configAttrs)

			attrs := map[string]string{
				"id":                 id,
				"primary_email":      "user@example.com",
				"name.#":             "1",
				"name.0.family_name": "Scott",
				"name.0.given_name":  "Michael",
			}
			for k, v := range test.state {
				attrs[k] = v
			}
			state := &terraform.InstanceState{ID: id, Attributes: attrs, RawConfig: rawConfig}

			config := terraform.NewResourceConfigShimmed(rawConfig, r.CoreConfigSchema())
			diags := r.Validate(config)
			if test.wantValidateErr != "" {
				if !diags.HasError() || !strings.Contains(diags[0].Summary+diags[0].Detail, test.wantValidateErr) {
					t.Fatalf("expected validation error %q, got %+v", test.wantValidateErr, diags)
				}
				return
			}
			if diags.HasError() {
				t.Fatalf("unexpected validation error: %+v", diags)
			}

			instanceDiff, err := r.Diff(context.Background(), state, config, nil)
			if err != nil {
				t.Fatalf("Diff: %s", err)
			}

			if instanceDiff == nil || instanceDiff.Empty() {
				if test.wantSend {
					t.Fatal("expected a diff, got none")
				}
				return
			}
			instanceDiff.RawConfig = rawConfig

			if _, diags := r.Apply(context.Background(), state, instanceDiff, nil); diags.HasError() {
				t.Fatalf("Apply: %+v", diags)
			}

			if !called {
				t.Fatal("Apply did not call the update function")
			}

			if gotValue != test.wantValue || gotSend != test.wantSend {
				t.Errorf("update sends (%q, %t), want (%q, %t)", gotValue, gotSend, test.wantValue, test.wantSend)
			}
		})
	}
}
