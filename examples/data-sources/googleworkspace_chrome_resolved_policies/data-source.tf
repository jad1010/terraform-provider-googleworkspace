# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# The effective values of the chrome.users.* policies for an org unit, also the values that it inherits.
data "googleworkspace_chrome_resolved_policies" "users" {
  org_unit_id          = "id:01ab2c3d4efg56h"
  policy_schema_filter = "chrome.users.*"
}

locals {
  resolved_users = {
    for p in data.googleworkspace_chrome_resolved_policies.users.resolved_policies :
    p.schema_name => p
    if length(p.additional_target_keys) == 0
  }
}

# Assert a value: incognito mode is unavailable.
# Enum value names have a prefix (<PREFIX>_ENUM_<VALUE>). Confirm the full name with the enum_fields attribute of
# googleworkspace_chrome_policy_schema instead of guessing the prefix.
data "googleworkspace_chrome_policy_schema" "incognito" {
  schema_name = "chrome.users.IncognitoMode"
}

locals {
  # null if the schema has no such value, so that a schema change fails the check, not the plan.
  incognito_unavailable = try(one(flatten([
    for f in data.googleworkspace_chrome_policy_schema.incognito.enum_fields : [
      for v in f.values : v.value if endswith(v.value, "_UNAVAILABLE")
    ] if f.field == "incognitoModeAvailability"
  ])), null)
}

check "incognito_mode_unavailable" {
  assert {
    # The API can omit a field at its default value, so read it with try() or lookup() and compare decoded values.
    condition = local.incognito_unavailable != null && try(
      jsondecode(local.resolved_users["chrome.users.IncognitoMode"].schema_values["incognitoModeAvailability"]),
      null,
    ) == local.incognito_unavailable
    error_message = "chrome.users.IncognitoMode must make incognito mode unavailable."
  }
}

# Assert that a policy is not set in the org unit or in a parent org unit. The result can still have the policy with
# the default value for the customer: then source_org_unit_id is empty.
# A read error of a data source in a check block is a warning, not an error, as is a failed assertion.
check "hsts_bypass_list_not_set" {
  data "googleworkspace_chrome_resolved_policies" "hsts" {
    org_unit_id          = "id:01ab2c3d4efg56h"
    policy_schema_filter = "chrome.users.HstsPolicyBypassList"
  }

  assert {
    condition = alltrue([
      for p in data.googleworkspace_chrome_resolved_policies.hsts.resolved_policies : p.source_org_unit_id == ""
    ])
    error_message = "chrome.users.HstsPolicyBypassList must not be set."
  }
}

# The policies that the org unit inherits, and the org unit that sets each one.
output "inherited_policies" {
  value = {
    for name, p in local.resolved_users : name => p.source_org_unit_id
    if p.inherited
  }
}
