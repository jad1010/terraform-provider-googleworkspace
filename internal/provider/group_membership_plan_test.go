// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/go-cty/cty/msgpack"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// planThroughProviderServer plans a change of an existing resource through
// the gRPC provider server, as Terraform does. The provider uses meta.
// It returns the attributes that require a replacement.
func planThroughProviderServer(t *testing.T, meta interface{}, typeName string, prior, config, proposed map[string]cty.Value) []string {
	t.Helper()

	p := New("dev")()
	p.SetMeta(meta)
	ty := p.ResourcesMap[typeName].CoreConfigSchema().ImpliedType()

	value := func(attrs map[string]cty.Value) *tfprotov5.DynamicValue {
		m := map[string]cty.Value{}
		for k, at := range ty.AttributeTypes() {
			m[k] = cty.NullVal(at)
		}
		for k, v := range attrs {
			m[k] = v
		}
		b, err := msgpack.Marshal(cty.ObjectVal(m), ty)
		if err != nil {
			t.Fatal(err)
		}
		return &tfprotov5.DynamicValue{MsgPack: b}
	}

	resp, err := schema.NewGRPCProviderServer(p).PlanResourceChange(context.Background(), &tfprotov5.PlanResourceChangeRequest{
		TypeName:         typeName,
		PriorState:       value(prior),
		ProposedNewState: value(proposed),
		Config:           value(config),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov5.DiagnosticSeverityError {
			t.Fatalf("PlanResourceChange() error: %s: %s", d.Summary, d.Detail)
		}
	}
	var replace []string
	for _, path := range resp.RequiresReplace {
		replace = append(replace, path.String())
	}
	return replace
}

func groupMemberValues(groupId, email string, computed bool) map[string]cty.Value {
	v := map[string]cty.Value{
		"group_id":          cty.StringVal(groupId),
		"email":             cty.StringVal(email),
		"role":              cty.StringVal("MEMBER"),
		"type":              cty.StringVal("USER"),
		"delivery_settings": cty.StringVal("ALL_MAIL"),
	}
	if computed {
		v["id"] = cty.StringVal("groups/" + groupId + "/members/ID1")
		v["member_id"] = cty.StringVal("ID1")
		v["status"] = cty.StringVal("ACTIVE")
		v["etag"] = cty.StringVal("etag-1")
	}
	return v
}

func groupMembersValues(groupId, email string, computed bool) map[string]cty.Value {
	member := map[string]cty.Value{
		"email":             cty.StringVal(email),
		"role":              cty.StringVal("MEMBER"),
		"type":              cty.StringVal("USER"),
		"delivery_settings": cty.StringVal("ALL_MAIL"),
		"status":            cty.NullVal(cty.String),
		"id":                cty.NullVal(cty.String),
	}
	v := map[string]cty.Value{"group_id": cty.StringVal(groupId)}
	if computed {
		member["status"] = cty.StringVal("ACTIVE")
		member["id"] = cty.StringVal("ID1")
		v["id"] = cty.StringVal("groups/" + groupId)
	}
	v["members"] = cty.SetVal([]cty.Value{cty.ObjectVal(member)})
	return v
}

// Prior state is what an earlier provider version wrote. A group_id that
// names the same group must not plan a replacement, in particular after an
// import that names the group by ID while the configuration uses its email.
func TestGroupMembershipPlan_groupIdThroughProviderServer(t *testing.T) {
	t.Parallel()

	f := newFakeDirectory()
	f.groups = map[string]string{"group@example.com": "G1", "g1": "G1", "other@example.com": "G2"}
	meta := f.meta(t)

	tests := map[string]struct {
		prior, config string
		replace       bool
	}{
		"same value":                       {prior: "group@example.com", config: "group@example.com"},
		"case only":                        {prior: "group@example.com", config: "Group@Example.COM"},
		"imported by ID, config by email":  {prior: "G1", config: "group@example.com"},
		"config switches from email to ID": {prior: "group@example.com", config: "G1"},
		"another group":                    {prior: "group@example.com", config: "other@example.com", replace: true},
	}

	resources := map[string]func(groupId, email string, computed bool) map[string]cty.Value{
		"googleworkspace_group_member":  groupMemberValues,
		"googleworkspace_group_members": groupMembersValues,
	}
	for typeName, values := range resources {
		for name, test := range tests {
			t.Run(typeName+"/"+name, func(t *testing.T) {
				replace := planThroughProviderServer(t, meta, typeName,
					values(test.prior, "alice@example.com", true),
					values(test.config, "Alice@Example.com", false),
					values(test.config, "Alice@Example.com", true),
				)
				if test.replace != (len(replace) > 0) {
					t.Errorf("RequiresReplace = %v, want replacement %v", replace, test.replace)
				}
			})
		}
	}
}
