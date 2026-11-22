// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	ctyjson "github.com/hashicorp/go-cty/cty/json"
	"github.com/hashicorp/go-cty/cty/msgpack"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

const gmailUserSettingsType = "googleworkspace_gmail_user_settings"

// gmailGRPC sends plan and apply requests to the provider through the gRPC
// server of the SDK, like Terraform does. Unlike the legacy test helpers, this
// path sets the raw configuration that the resource reads.
type gmailGRPC struct {
	t      *testing.T
	server *schema.GRPCProviderServer
	ty     cty.Type
}

func newGmailGRPC(t *testing.T) *gmailGRPC {
	t.Helper()

	p := New("test")()
	p.SetMeta(&apiClient{})

	return &gmailGRPC{
		t:      t,
		server: schema.NewGRPCProviderServer(p),
		ty:     p.ResourcesMap[gmailUserSettingsType].CoreConfigSchema().ImpliedType(),
	}
}

func (g *gmailGRPC) value(object map[string]interface{}) cty.Value {
	g.t.Helper()

	if object == nil {
		return cty.NullVal(g.ty)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		g.t.Fatalf("encoding object: %s", err)
	}
	value, err := ctyjson.Unmarshal(raw, g.ty)
	if err != nil {
		g.t.Fatalf("converting object: %s", err)
	}
	return value
}

func (g *gmailGRPC) object(value cty.Value) map[string]interface{} {
	g.t.Helper()

	raw, err := ctyjson.Marshal(value, g.ty)
	if err != nil {
		g.t.Fatalf("encoding value: %s", err)
	}
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil {
		g.t.Fatalf("decoding value: %s", err)
	}
	return object
}

func (g *gmailGRPC) dynamic(value cty.Value) *tfprotov5.DynamicValue {
	g.t.Helper()

	raw, err := msgpack.Marshal(value, g.ty)
	if err != nil {
		g.t.Fatalf("encoding value: %s", err)
	}
	return &tfprotov5.DynamicValue{MsgPack: raw}
}

func (g *gmailGRPC) decode(value *tfprotov5.DynamicValue) cty.Value {
	g.t.Helper()

	decoded, err := msgpack.Unmarshal(value.MsgPack, g.ty)
	if err != nil {
		g.t.Fatalf("decoding value: %s", err)
	}
	return decoded
}

// proposedNew does what Terraform does before a plan: an optional and
// computed attribute that is not in the configuration keeps its prior value.
func (g *gmailGRPC) proposedNew(prior cty.Value, config map[string]interface{}) map[string]interface{} {
	g.t.Helper()

	raw, err := json.Marshal(config)
	if err != nil {
		g.t.Fatalf("encoding config: %s", err)
	}
	var proposed map[string]interface{}
	if err := json.Unmarshal(raw, &proposed); err != nil {
		g.t.Fatalf("decoding config: %s", err)
	}
	if prior.IsNull() {
		return proposed
	}

	priorObject := g.object(prior)
	proposed["id"] = priorObject["id"]
	computed := []string{"auto_expunge", "expunge_behavior", "max_folder_size", "disposition"}
	for _, block := range gmailUserSettingsBlocks {
		configList, _ := proposed[block].([]interface{})
		priorList, _ := priorObject[block].([]interface{})
		if len(configList) != 1 || len(priorList) != 1 {
			continue
		}
		configBlock := configList[0].(map[string]interface{})
		priorBlock := priorList[0].(map[string]interface{})
		for _, key := range computed {
			if value, ok := priorBlock[key]; ok && configBlock[key] == nil {
				configBlock[key] = value
			}
		}
	}
	return proposed
}

func (g *gmailGRPC) checkDiagnostics(diagnostics []*tfprotov5.Diagnostic) {
	g.t.Helper()

	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == tfprotov5.DiagnosticSeverityError {
			g.t.Fatalf("unexpected error: %s: %s", diagnostic.Summary, diagnostic.Detail)
		}
	}
}

func (g *gmailGRPC) plan(prior cty.Value, config map[string]interface{}) *tfprotov5.PlanResourceChangeResponse {
	g.t.Helper()

	resp, err := g.server.PlanResourceChange(context.Background(), &tfprotov5.PlanResourceChangeRequest{
		TypeName:         gmailUserSettingsType,
		PriorState:       g.dynamic(prior),
		ProposedNewState: g.dynamic(g.value(g.proposedNew(prior, config))),
		Config:           g.dynamic(g.value(config)),
	})
	if err != nil {
		g.t.Fatalf("plan: %s", err)
	}
	g.checkDiagnostics(resp.Diagnostics)
	return resp
}

func (g *gmailGRPC) apply(prior cty.Value, config map[string]interface{}) cty.Value {
	g.t.Helper()

	plan := g.plan(prior, config)
	resp, err := g.server.ApplyResourceChange(context.Background(), &tfprotov5.ApplyResourceChangeRequest{
		TypeName:       gmailUserSettingsType,
		PriorState:     g.dynamic(prior),
		PlannedState:   plan.PlannedState,
		Config:         g.dynamic(g.value(config)),
		PlannedPrivate: plan.PlannedPrivate,
	})
	if err != nil {
		g.t.Fatalf("apply: %s", err)
	}
	g.checkDiagnostics(resp.Diagnostics)

	state := g.decode(resp.NewState)
	if planned := g.decode(g.plan(state, config).PlannedState); !planned.RawEquals(state) {
		g.t.Fatalf("unexpected change after apply:\nstate:   %#v\nplanned: %#v", g.object(state), g.object(planned))
	}
	return state
}

func gmailGRPCConfig(blocks map[string]interface{}) map[string]interface{} {
	config := map[string]interface{}{"user_email": testGmailUserEmail}
	for _, block := range gmailUserSettingsBlocks {
		config[block] = []interface{}{}
		if values, ok := blocks[block]; ok {
			config[block] = []interface{}{values}
		}
	}
	return config
}

func TestGmailUserSettingsGRPC_rawConfig(t *testing.T) {
	f := newFakeGmail(t)
	g := newGmailGRPC(t)

	// Create: auto_expunge and expunge_behavior are not set and keep the
	// current values.
	state := g.apply(cty.NullVal(g.ty), gmailGRPCConfig(map[string]interface{}{
		"imap": map[string]interface{}{"enabled": false, "max_folder_size": 1000},
	}))
	assertBody(t, f.lastPut(t, "imap"), map[string]interface{}{
		"enabled":         false,
		"autoExpunge":     true,
		"expungeBehavior": "archive",
		"maxFolderSize":   float64(1000),
	})
	f.calls()

	// Update without max_folder_size in the configuration keeps 1000.
	state = g.apply(state, gmailGRPCConfig(map[string]interface{}{
		"imap": map[string]interface{}{"enabled": true},
	}))
	assertBody(t, f.lastPut(t, "imap"), map[string]interface{}{"enabled": true, "maxFolderSize": float64(1000)})
	f.calls()

	// An explicit false and 0 are sent.
	state = g.apply(state, gmailGRPCConfig(map[string]interface{}{
		"imap": map[string]interface{}{"enabled": true, "auto_expunge": false, "max_folder_size": 0},
	}))
	assertBody(t, f.lastPut(t, "imap"), map[string]interface{}{"autoExpunge": false, "maxFolderSize": float64(0)})
	f.calls()

	// Removing the imap block and adding the pop block writes only POP.
	state = g.apply(state, gmailGRPCConfig(map[string]interface{}{
		"pop": map[string]interface{}{"access_window": "disabled"},
	}))
	assertBody(t, f.lastPut(t, "pop"), map[string]interface{}{"accessWindow": "disabled", "disposition": "leaveInInbox"})
	if got := f.calls(); !containsOnlySetting(got, "pop") {
		t.Fatalf("API calls = %v, want only POP calls", got)
	}
	if imap := state.GetAttr("imap"); imap.LengthInt() != 0 {
		t.Fatalf("imap = %#v, want no block", imap)
	}
}

func TestGmailUserSettingsGRPC_disabledForwardingOmitsValues(t *testing.T) {
	f := newFakeGmail(t)
	f.dropWhenDisabled = true
	g := newGmailGRPC(t)

	// apply fails if the plan after the apply has a change.
	state := g.apply(cty.NullVal(g.ty), gmailGRPCConfig(map[string]interface{}{
		"auto_forwarding": map[string]interface{}{"enabled": false, "disposition": "leaveInInbox"},
	}))
	got := state.GetAttr("auto_forwarding").Index(cty.NumberIntVal(0)).GetAttr("disposition")
	if !got.RawEquals(cty.StringVal("leaveInInbox")) {
		t.Fatalf("auto_forwarding.0.disposition = %#v, want leaveInInbox", got)
	}
}

func containsOnlySetting(calls []string, setting string) bool {
	for _, call := range calls {
		if call != "GET "+setting && call != "PUT "+setting {
			return false
		}
	}
	return len(calls) > 0
}
