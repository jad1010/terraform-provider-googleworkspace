// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// The API returns these values in upper case. A lower-case value in the
// configuration would pass a case-insensitive check and then show a diff on
// every plan, so the new fields reject it at validation time.
func TestResourceGroupSettings_newEnumFieldsAreCaseSensitive(t *testing.T) {
	r := resourceGroupSettings()
	for field, valid := range map[string]string{
		"default_sender":          "GROUP",
		"who_can_approve_members": "NONE_CAN_APPROVE",
		"who_can_ban_users":       "OWNERS_ONLY",
	} {
		validate := r.Schema[field].ValidateDiagFunc
		if diags := validate(valid, cty.GetAttrPath(field)); diags.HasError() {
			t.Errorf("%s: %q should be valid: %v", field, valid, diags)
		}
		lower := strings.ToLower(valid)
		if diags := validate(lower, cty.GetAttrPath(field)); !diags.HasError() {
			t.Errorf("%s: %q should be rejected", field, lower)
		}
	}
}

var newGroupSettingsFields = []string{"DefaultSender", "WhoCanApproveMembers", "WhoCanBanUsers"}

func TestBuildGroupSettingsCreate_favoriteRepliesOnTop(t *testing.T) {
	cases := []struct {
		name      string
		cfg       map[string]interface{}
		wantValue string
		wantForce bool
	}{
		{"unset", map[string]interface{}{"email": "g@example.com"}, "", false},
		{"false", map[string]interface{}{"email": "g@example.com", "favorite_replies_on_top": false}, "false", true},
		{"true", map[string]interface{}{"email": "g@example.com", "favorite_replies_on_top": true}, "true", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := groupSettingsData(t, nil, tc.cfg)
			obj := buildGroupSettingsCreate(d)
			if obj.FavoriteRepliesOnTop != tc.wantValue {
				t.Errorf("FavoriteRepliesOnTop = %q, want %q", obj.FavoriteRepliesOnTop, tc.wantValue)
			}
			if got := slices.Contains(obj.ForceSendFields, "FavoriteRepliesOnTop"); got != tc.wantForce {
				t.Errorf("FavoriteRepliesOnTop in ForceSendFields = %v, want %v", got, tc.wantForce)
			}
		})
	}
}

func TestBuildGroupSettingsCreate_unsetStringFieldsKeepAPIDefault(t *testing.T) {
	d := groupSettingsData(t, nil, map[string]interface{}{"email": "g@example.com"})
	obj := buildGroupSettingsCreate(d)
	if obj.DefaultSender != "" || obj.WhoCanApproveMembers != "" || obj.WhoCanBanUsers != "" {
		t.Errorf("unset fields must stay empty, got %q %q %q", obj.DefaultSender, obj.WhoCanApproveMembers, obj.WhoCanBanUsers)
	}
	for _, f := range newGroupSettingsFields {
		if slices.Contains(obj.ForceSendFields, f) {
			t.Errorf("%s must not be in ForceSendFields on Create", f)
		}
	}
}

// groupSettingsData builds ResourceData from an optional prior state and a
// configuration. RawConfig is set because Create reads it to tell an unset
// attribute from a false one, and TestResourceDataRaw does not set it.
func groupSettingsData(t *testing.T, state map[string]string, cfg map[string]interface{}) *schema.ResourceData {
	t.Helper()
	sm := schema.InternalMap(resourceGroupSettings().Schema)

	var st *terraform.InstanceState
	if state != nil {
		st = &terraform.InstanceState{ID: "g@example.com", Attributes: state}
	}
	diff, err := sm.Diff(context.Background(), st, terraform.NewResourceConfigRaw(cfg), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	raw := map[string]cty.Value{}
	for name, sch := range resourceGroupSettings().Schema {
		typ := cty.String
		if sch.Type == schema.TypeBool {
			typ = cty.Bool
		}
		raw[name] = cty.NullVal(typ)
		switch v := cfg[name].(type) {
		case string:
			raw[name] = cty.StringVal(v)
		case bool:
			raw[name] = cty.BoolVal(v)
		}
	}
	diff.RawConfig = cty.ObjectVal(raw)

	d, err := sm.Data(st, diff)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBuildGroupSettingsUpdate_favoriteRepliesOnTop(t *testing.T) {
	state := map[string]string{"email": "g@example.com", "favorite_replies_on_top": "true"}

	d := groupSettingsData(t, state, map[string]interface{}{"email": "g@example.com", "favorite_replies_on_top": false})
	obj := buildGroupSettingsUpdate(d)
	if obj.FavoriteRepliesOnTop != "false" {
		t.Errorf("FavoriteRepliesOnTop = %q, want \"false\"", obj.FavoriteRepliesOnTop)
	}
	if !slices.Contains(obj.ForceSendFields, "FavoriteRepliesOnTop") {
		t.Error("FavoriteRepliesOnTop missing from ForceSendFields after true to false change")
	}

	d = groupSettingsData(t, state, map[string]interface{}{"email": "g@example.com", "favorite_replies_on_top": true})
	obj = buildGroupSettingsUpdate(d)
	if obj.FavoriteRepliesOnTop != "" || slices.Contains(obj.ForceSendFields, "FavoriteRepliesOnTop") {
		t.Errorf("unchanged field must not be sent, got %q %v", obj.FavoriteRepliesOnTop, obj.ForceSendFields)
	}
}
