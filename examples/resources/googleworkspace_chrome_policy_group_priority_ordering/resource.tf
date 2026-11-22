# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

resource "googleworkspace_group" "security" {
  email = "chrome-security@example.com"
}

resource "googleworkspace_group" "pilot" {
  email = "chrome-pilot@example.com"
}

# Both groups set the policy of an app. The value of the first group in group_ids applies to users in both groups.
resource "googleworkspace_chrome_group_policy" "security" {
  group_id = googleworkspace_group.security.id
  additional_target_keys {
    target_key   = "app_id"
    target_value = "chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi"
  }
  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode("BLOCKED")
    }
  }
}

resource "googleworkspace_chrome_group_policy" "pilot" {
  group_id = googleworkspace_group.pilot.id
  additional_target_keys {
    target_key   = "app_id"
    target_value = "chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi"
  }
  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode("FORCED")
    }
  }
}

resource "googleworkspace_chrome_policy_group_priority_ordering" "docs_offline" {
  policy_namespace = "chrome.users.apps"

  policy_target_key {
    additional_target_key_names = {
      app_id = "chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi"
    }
  }

  # Highest priority first. List every group that sets the policy for the app: the API rejects a partial list.
  group_ids = [
    googleworkspace_group.security.id,
    googleworkspace_group.pilot.id,
  ]

  depends_on = [
    googleworkspace_chrome_group_policy.security,
    googleworkspace_chrome_group_policy.pilot,
  ]
}
