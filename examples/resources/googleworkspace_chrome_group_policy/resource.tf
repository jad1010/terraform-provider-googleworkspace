# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

resource "googleworkspace_group" "chrome_pilot" {
  email = "chrome-pilot@example.com"
}

# Force-install an extension for the members of a group.
resource "googleworkspace_chrome_group_policy" "docs_offline" {
  group_id = googleworkspace_group.chrome_pilot.id

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
