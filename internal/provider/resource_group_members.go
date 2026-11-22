// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	directory "google.golang.org/api/admin/directory/v1"
)

const deliverySettingsDefault = "ALL_MAIL"

type MemberChange struct {
	Old, New map[string]interface{}
}

func resourceGroupMembers() *schema.Resource {
	memberResource := &schema.Resource{
		Schema: map[string]*schema.Schema{
			"email": {
				Description: "The member's email address. A member can be a user or another group. This property is" +
					"required when adding a member to a group. The email must be unique and cannot be an alias of " +
					"another group. If the email address is changed, the API automatically reflects the email address changes." +
					caseInsensitiveNote,
				Type:     schema.TypeString,
				Required: true,
				// The API stores email addresses in lower case.
				DiffSuppressFunc: emailDiffSuppress,
			},
			"role": {
				Description: "The member's role in a group. The API returns an error for cycles in group memberships. " +
					"For example, if group1 is a member of group2, group2 cannot be a member of group1. " +
					"Acceptable values are: " +
					"\n\t- `MANAGER`: This role is only available if the Google Groups for Business is " +
					"enabled using the Admin Console. A `MANAGER` role can do everything done by an `OWNER` role except " +
					"make a member an `OWNER` or delete the group. A group can have multiple `MANAGER` members. " +
					"\n\t- `MEMBER`: This role can subscribe to a group, view discussion archives, and view the group's " +
					"membership list. " +
					"\n\t- `OWNER`: This role can send messages to the group, add or remove members, change member roles, " +
					"change group's settings, and delete the group. An OWNER must be a member of the group. " +
					"A group can have more than one OWNER.",
				Type:     schema.TypeString,
				Optional: true,
				Default:  "MEMBER",
				ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{"MANAGER", "MEMBER", "OWNER"},
					false)),
			},
			"type": {
				Description: "The type of group member. Acceptable values are: " +
					"\n\t- `CUSTOMER`: The member represents all users in a domain. An email address is not returned and the " +
					"ID returned is the customer ID. " +
					"\n\t- `GROUP`: The member is another group. " +
					"\n\t- `USER`: The member is a user.",
				Type:     schema.TypeString,
				Optional: true,
				Default:  "USER",
				ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{"CUSTOMER", "GROUP", "USER"},
					false)),
			},
			"delivery_settings": {
				Description: "Defines mail delivery preferences of member. Acceptable values are:" +
					"\n\t- `ALL_MAIL`: All messages, delivered as soon as they arrive. " +
					"\n\t- `DAILY`: No more than one message a day. " +
					"\n\t- `DIGEST`: Up to 25 messages bundled into a single message. " +
					"\n\t- `DISABLED`: Remove subscription. " +
					"\n\t- `NONE`: No messages.",
				Type:     schema.TypeString,
				Optional: true,
				Default:  deliverySettingsDefault,
				ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{"ALL_MAIL", "DAILY", "DIGEST",
					"DISABLED", "NONE"}, false)),
			},
			"status": {
				Description: "Status of member.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"id": {
				Description: "The unique ID of the group member. A member id can be used as a member request URI's memberKey.",
				Type:        schema.TypeString,
				Computed:    true,
			},
		},
	}

	return &schema.Resource{
		// This description is used by the documentation generator and the language server.
		Description: "Group Members resource manages Google Workspace Groups Members. Group Members resides under the " +
			"`https://www.googleapis.com/auth/admin.directory.group` client scope. After a create or update, the " +
			"resource waits up to 5 minutes for the API to list the members that it wrote. If the wait ends first, " +
			"the resource gives a warning and records the membership that the API lists. If a declared member already exists in the group, the resource adopts it, sets its role and " +
			"delivery settings, and gives a warning. The resource is authoritative: Read records every listed member, " +
			"so destroying or replacing the resource removes all members of the group, also members that the " +
			"configuration does not declare. Two resources that declare the same member both succeed and both manage it.",

		CreateContext: resourceGroupMembersCreate,
		CustomizeDiff: forceNewIfOtherGroup,
		ReadContext:   resourceGroupMembersRead,
		UpdateContext: resourceGroupMembersUpdate,
		DeleteContext: resourceGroupMembersDelete,

		Importer: &schema.ResourceImporter{
			StateContext: resourceGroupMembersImport,
		},

		Schema: map[string]*schema.Schema{
			"group_id": {
				Description: "Identifies the group in the API request. The value can be the group's email address, " +
					"group alias, or the unique group ID." + groupIDNote,
				Type:     schema.TypeString,
				Required: true,
				// forceNewIfOtherGroup replaces the resource when the new value
				// names another group. A group email or alias can differ in case
				// only. A replacement would delete and add again every member.
				DiffSuppressFunc: emailDiffSuppress,
			},
			"members": {
				Description: "The members of the group",
				Type:        schema.TypeSet,
				Optional:    true,
				Elem:        memberResource,
				// Hash the email in lower case, so that a member keeps its set
				// element when only the case of its email changes.
				Set: func(v interface{}) int {
					m := make(map[string]interface{})
					for k, val := range v.(map[string]interface{}) {
						m[k] = val
					}
					if email, ok := m["email"].(string); ok {
						m["email"] = strings.ToLower(email)
					}
					return schema.HashResource(memberResource)(m)
				},
			},

			"etag": {
				Description: "ETag of the resource.",
				Type:        schema.TypeString,
				Computed:    true,
			},

			// Adding a computed id simply to override the `optional` id that gets added in the SDK
			// that will then display improperly in the docs
			"id": {
				Description: "The ID of this resource.",
				Type:        schema.TypeString,
				Computed:    true,
			},
		},
	}
}

func resourceGroupMembersCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)
	groupId := d.Get("group_id").(string)

	log.Printf("[DEBUG] Creating Group Members in group %s", groupId)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	membersService, diags := GetMembersService(directoryService)
	if diags.HasError() {
		return diags
	}

	members := d.Get("members").(*schema.Set)
	expected := make([]groupMemberExpectation, 0, members.Len())
	for _, mMap := range members.List() {
		memb := mMap.(map[string]interface{})

		memberObj := directory.Member{
			Email:            memb["email"].(string),
			Role:             memb["role"].(string),
			Type:             memb["type"].(string),
			DeliverySettings: memb["delivery_settings"].(string),
		}

		log.Printf("[DEBUG] Creating Group Member %q in group %s: %#v", memberObj.Email, groupId, memberObj.Email)

		// Members inserted before a failure are not saved to state: a
		// tainted resource would be destroyed, with members that existed
		// before Terraform. The next apply adopts them (409).
		inserted, adoption, err := insertGroupMember(ctx, membersService, groupId, &memberObj)
		if err != nil {
			return append(diags, diag.FromErr(err)...)
		}
		diags = append(diags, adoption...)
		expected = append(expected, groupMemberExpectation{email: memberObj.Email, id: inserted.Id, role: memberObj.Role})
	}

	d.SetId(fmt.Sprintf("groups/%s", groupId))

	// INSERT is eventually consistent: a listing made right away can omit a
	// member just inserted, and Read would record that stale listing.
	// For the same reason as above, an error after the inserts saves no
	// state (no ID), and the next apply adopts the members.
	diags = append(diags, waitForGroupMembersWrites(ctx, membersService, groupId, expected)...)
	if diags.HasError() {
		d.SetId("")
		return diags
	}

	diags = append(diags, resourceGroupMembersRead(ctx, d, meta)...)
	if diags.HasError() {
		d.SetId("")
	}
	return diags
}

func resourceGroupMembersRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	membersService, diags := GetMembersService(directoryService)
	if diags.HasError() {
		return diags
	}

	groupId := d.Get("group_id").(string)
	// include_derived_membership is a read-only option available in the datasource, but not in the resource
	// because the datasource and resource share the same Read function, we can add it in here, but need to
	// make sure it is always false for the resource
	includeDerivedMembership := false
	if includeDM, ok := d.GetOk("include_derived_membership"); ok {
		includeDerivedMembership = includeDM.(bool)
	}
	result, err := listGroupMembers(ctx, membersService, groupId, includeDerivedMembership)
	if err != nil {
		return handleNotFoundError(err, d, d.Id())
	}

	configMembers := d.Get("members").(*schema.Set)

	members := make([]interface{}, len(result))
	for i, member := range result {

		// Use value if present or default as "delivery_settings" is not provided by API
		deliverySettings := deliverySettingsDefault
		// The API returns the email in lower case. Keep the spelling from
		// the configuration or state when it differs only in case.
		email := member.Email

		for _, cm := range configMembers.List() {
			cMem := cm.(map[string]interface{})
			if member.Email != "" && strings.EqualFold(cMem["email"].(string), member.Email) {
				email = cMem["email"].(string)
				if cMem["delivery_settings"] == "" {
					continue
				}

				deliverySettings = cMem["delivery_settings"].(string)
				break
			}
		}

		members[i] = map[string]interface{}{
			"email":             email,
			"role":              member.Role,
			"type":              member.Type,
			"status":            member.Status,
			"delivery_settings": deliverySettings,
			"id":                member.Id,
		}
	}

	if err := d.Set("members", members); err != nil {
		diags = append(diags, diag.Diagnostic{
			Severity:      diag.Error,
			Summary:       "Error setting attribute",
			Detail:        err.Error(),
			AttributePath: cty.IndexStringPath("members"),
		})
	}

	d.SetId(fmt.Sprintf("groups/%s", groupId))

	return diags
}

func resourceGroupMembersUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	groupId := d.Get("group_id").(string)
	log.Printf("[DEBUG] Updating Group Members of group: %s", groupId)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	membersService, diags := GetMembersService(directoryService)
	if diags.HasError() {
		return diags
	}

	o, n := d.GetChange("members")
	vals := make(map[string]*MemberChange)
	// The API treats email addresses without regard to case, so a change that
	// only changes the case of an email is not a delete and an insert.
	for _, raw := range o.(*schema.Set).List() {
		obj := raw.(map[string]interface{})
		k := strings.ToLower(obj["email"].(string))
		vals[k] = &MemberChange{Old: obj}
	}
	for _, raw := range n.(*schema.Set).List() {
		obj := raw.(map[string]interface{})
		k := strings.ToLower(obj["email"].(string))
		if _, ok := vals[k]; !ok {
			vals[k] = &MemberChange{}
		}
		vals[k].New = obj
	}

	// On an error, keep the prior state: the planned members have no IDs,
	// and the next plan computes the remaining changes again.
	fail := func(err error) diag.Diagnostics {
		// State keeps the pre-update members until the next refresh (intended).
		d.Partial(true)
		return append(diags, diag.FromErr(err)...)
	}

	// Phase 1 inserts and updates; phase 2 deletes. Two state entries can
	// name the same member: an alias in the configuration, or a renamed
	// user whose state has the new primary address. The insert then adopts
	// the member (409), and the delete of the old entry must not remove it.
	var expected []groupMemberExpectation
	written := map[string]bool{}
	var deletes []*MemberChange
	for _, change := range vals {
		if change.New == nil {
			deletes = append(deletes, change)
			continue
		}
		// no change: the email can differ only in case, and status and id
		// are computed
		if change.Old != nil &&
			change.Old["role"] == change.New["role"] &&
			change.Old["type"] == change.New["type"] &&
			change.Old["delivery_settings"] == change.New["delivery_settings"] {
			continue
		}

		memberObj := directory.Member{
			Email:            change.New["email"].(string),
			Role:             change.New["role"].(string),
			Type:             change.New["type"].(string),
			DeliverySettings: change.New["delivery_settings"].(string),
		}

		var member *directory.Member
		var err error
		if change.Old != nil {
			member, err = membersService.Update(groupId, change.Old["id"].(string), &memberObj).Context(ctx).Do()
			if isNotFound(err) {
				// The member was removed outside Terraform. The
				// configuration still declares it, so insert it again.
				log.Printf("[DEBUG] Group Member %q is gone from group %s; inserting it again", memberObj.Email, groupId)
				member = nil
			} else if err != nil {
				return fail(err)
			}
		}
		if member == nil {
			log.Printf("[DEBUG] Creating Group Member %q in group %s: %#v", memberObj.Email, groupId, memberObj.Email)
			var adoption diag.Diagnostics
			member, adoption, err = insertGroupMember(ctx, membersService, groupId, &memberObj)
			if err != nil {
				return fail(err)
			}
			diags = append(diags, adoption...)
		}
		id := member.Id
		if id == "" && change.Old != nil {
			id = change.Old["id"].(string)
		}
		if id != "" {
			written[id] = true
		}
		expected = append(expected, groupMemberExpectation{email: memberObj.Email, id: id, role: memberObj.Role})
	}

	for _, change := range deletes {
		memberKey := change.Old["id"].(string)
		email := change.Old["email"].(string)
		if written[memberKey] {
			log.Printf("[DEBUG] Not removing Group Member %q (%s) from group %s: another declared address names it", email, memberKey, groupId)
			continue
		}
		log.Printf("[DEBUG] Remove Group Member %q from group %s: %#v", email, groupId, memberKey)
		err := membersService.Delete(groupId, memberKey).Context(ctx).Do()
		if err != nil && !isNotFound(err) {
			return fail(err)
		}
		// A 404 means the member is already gone.
		expected = append(expected, groupMemberExpectation{email: email, id: memberKey, absent: true})
	}

	d.SetId(fmt.Sprintf("groups/%s", groupId))
	log.Printf("[DEBUG] Finished updating Group Members %q", groupId)

	// Inserts, updates and deletes are eventually consistent: wait until the
	// listing shows them before Read records it.
	diags = append(diags, waitForGroupMembersWrites(ctx, membersService, groupId, expected)...)
	if diags.HasError() {
		// State keeps the pre-update members until the next refresh (intended).
		d.Partial(true)
		return diags
	}

	return append(diags, resourceGroupMembersRead(ctx, d, meta)...)
}

func resourceGroupMembersDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	groupId := d.Get("group_id").(string)
	members := d.Get("members").(*schema.Set)
	log.Printf("[DEBUG] Deleting Group Members from Group %s", groupId)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	membersService, diags := GetMembersService(directoryService)
	if diags.HasError() {
		return diags
	}

	// Try to delete every member. A member that is already gone (404) is
	// not an error. Other errors are reported together after the loop. A
	// canceled or expired ctx stops the loop with one error.
	list := members.List()
	stopped := func(i int) diag.Diagnostics {
		return append(diags, diag.Diagnostic{
			Severity: diag.Error,
			Summary:  fmt.Sprintf("Deleting members of group %s stopped: %s", groupId, ctx.Err()),
			Detail:   fmt.Sprintf("%d of %d members were not deleted or not checked.", len(list)-i, len(list)),
		})
	}
	for i, raw := range list {
		if ctx.Err() != nil {
			return stopped(i)
		}
		member := raw.(map[string]interface{})
		memberKey := member["id"].(string)
		err := membersService.Delete(groupId, memberKey).Context(ctx).Do()
		if err != nil && ctx.Err() != nil {
			return stopped(i)
		}
		if isNotFound(err) {
			log.Printf("[DEBUG] Group Member %q: %#v was already removed from group %s", memberKey, member["email"].(string), groupId)
			continue
		}
		if err != nil {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("Error deleting member %s (%s) from group %s", member["email"].(string), memberKey, groupId),
				Detail:   err.Error(),
			})
			continue
		}
		log.Printf("[DEBUG] Finished deleting Group Member %q: %#v", memberKey, member["email"].(string))
	}
	if diags.HasError() {
		return diags
	}

	log.Printf("[DEBUG] Finished deleting Group Members %s", groupId)

	return diags
}

// waitForGroupMembersWrites waits until the member listing shows the writes
// in expected. A wait that runs out of time gives a warning and not an error,
// because the writes succeeded. A listing error gives an error.
func waitForGroupMembersWrites(ctx context.Context, membersService *directory.MembersService, groupId string, expected []groupMemberExpectation) diag.Diagnostics {
	err := waitForGroupMembers(ctx, consistencyWaitBudget(ctx, groupMembersConsistencyTimeout), groupMembersPollInterval, expected,
		newGroupMembersSource(membersService, groupId))
	if isConsistencyTimeout(ctx, err) {
		log.Printf("[WARN] %s", err)
		return diag.Diagnostics{consistencyWarning("googleworkspace_group_members", err)}
	}
	if err != nil {
		return diag.FromErr(fmt.Errorf("waiting for members of group %s to be consistent: %w", groupId, err))
	}
	return nil
}

// emailDiffSuppress ignores a change that changes only the case of an email.
// The API stores email addresses in lower case.
func emailDiffSuppress(_, old, new string, _ *schema.ResourceData) bool {
	return strings.EqualFold(old, new)
}

func resourceGroupMembersImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	parts := strings.Split(d.Id(), "/")

	// id is of format "groups/<group_id>"
	if len(parts) != 2 {
		return nil, fmt.Errorf("Group Member Id (%s) is not of the correct format (groups/<group_id>)", d.Id())
	}

	d.Set("group_id", parts[1])

	return []*schema.ResourceData{d}, nil
}
