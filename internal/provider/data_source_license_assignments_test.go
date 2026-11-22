// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccDataSourceLicenseAssignments(t *testing.T) {
	t.Parallel()

	data := testAccLicenseAssignmentData(t)
	userEmail := fmt.Sprintf("%s@%s", data["userEmail"], data["domainName"])

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testAccCheckLicenseAssignmentDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccDataSourceLicenseAssignments(data),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.googleworkspace_license_assignments.sku", "id",
						fmt.Sprintf("%s/%s", data["productId"], data["skuId"])),
					resource.TestCheckTypeSetElemNestedAttrs("data.googleworkspace_license_assignments.sku",
						"license_assignments.*", map[string]string{
							"user_id":    userEmail,
							"product_id": data["productId"].(string),
							"sku_id":     data["skuId"].(string),
						}),
					resource.TestCheckResourceAttr("data.googleworkspace_license_assignments.product", "id",
						data["productId"].(string)),
					resource.TestCheckTypeSetElemNestedAttrs("data.googleworkspace_license_assignments.product",
						"license_assignments.*", map[string]string{
							"user_id": userEmail,
							"sku_id":  data["skuId"].(string),
						}),
				),
			},
		},
	})
}

func testAccDataSourceLicenseAssignments(data map[string]interface{}) string {
	return testAccLicenseAssignment(data) + Nprintf(`
data "googleworkspace_license_assignments" "sku" {
  product_id = "%{productId}"
  sku_id     = "%{skuId}"

  depends_on = [googleworkspace_license_assignment.test]
}

data "googleworkspace_license_assignments" "product" {
  product_id = "%{productId}"

  depends_on = [googleworkspace_license_assignment.test]
}
`, data)
}
