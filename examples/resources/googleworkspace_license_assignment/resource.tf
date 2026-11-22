# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# The licensing scope is not a default scope. Add it to oauth_scopes, together
# with the scopes that your other resources need.
provider "googleworkspace" {
  customer_id = "A01b123xz"
  oauth_scopes = [
    "https://www.googleapis.com/auth/admin.directory.user",
    "https://www.googleapis.com/auth/apps.licensing",
  ]
}

resource "googleworkspace_user" "dwight" {
  primary_email = "dwight.schrute@example.com"
  password      = "34819d7beeabb9260a5c854bc85b3e44"
  hash_function = "MD5"

  name {
    family_name = "Schrute"
    given_name  = "Dwight"
  }
}

# Google Workspace Business Starter
resource "googleworkspace_license_assignment" "dwight" {
  product_id = "Google-Apps"
  sku_id     = "1010020027"
  user_id    = googleworkspace_user.dwight.primary_email
}
