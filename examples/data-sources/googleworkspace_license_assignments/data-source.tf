# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# All license assignments of a product
data "googleworkspace_license_assignments" "workspace" {
  product_id = "Google-Apps"
}

# The license assignments of one SKU of a product
data "googleworkspace_license_assignments" "business-starter" {
  product_id = "Google-Apps"
  sku_id     = "1010020027"
}

output "business_starter_users" {
  value = data.googleworkspace_license_assignments.business-starter.license_assignments[*].user_id
}
