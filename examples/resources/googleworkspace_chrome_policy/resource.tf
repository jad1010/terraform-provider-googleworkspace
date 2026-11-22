# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

resource "googleworkspace_org_unit" "example" {
  name                 = "example"
  parent_org_unit_path = "/"
}

resource "googleworkspace_chrome_policy" "example" {
  org_unit_id = googleworkspace_org_unit.example.id
  policies {
    schema_name = "chrome.users.MaxConnectionsPerProxy"
    schema_values = {
      maxConnectionsPerProxy = jsonencode(34)
    }
  }
}

# App policies (chrome.users.apps.*) need the app in additional_target_keys.
# Use one resource for each app, for example to allow only reviewed extensions.
locals {
  extension_install_types = {
    # Google Docs Offline
    "chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi" = "FORCED"
  }
}

resource "googleworkspace_chrome_policy" "extensions" {
  for_each = local.extension_install_types

  org_unit_id = googleworkspace_org_unit.example.id

  additional_target_keys {
    target_key   = "app_id"
    target_value = each.key
  }

  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode(each.value)
    }
  }
}
