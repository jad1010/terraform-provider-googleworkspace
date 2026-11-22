// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccDataSourceChromeResolvedPolicies(t *testing.T) {
	t.Parallel()

	ouName := fmt.Sprintf("tf-test-%s", acctest.RandString(10))

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccDataSourceChromeResolvedPolicies(ouName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.#", "1"),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.0.schema_name", "chrome.users.MaxConnectionsPerProxy"),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.0.value", `{"maxConnectionsPerProxy":33}`),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.0.schema_values.maxConnectionsPerProxy", "33"),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.0.inherited", "false"),
					resource.TestCheckResourceAttrPair("data.googleworkspace_chrome_resolved_policies.test", "resolved_policies.0.source_org_unit_id", "googleworkspace_chrome_policy.test", "id"),
				),
			},
		},
	})
}

func testAccDataSourceChromeResolvedPolicies(ouName string) string {
	return fmt.Sprintf(`
resource "googleworkspace_org_unit" "test" {
  name = "%s"
  parent_org_unit_path = "/"
}

resource "googleworkspace_chrome_policy" "test" {
  org_unit_id = googleworkspace_org_unit.test.id
  policies {
    schema_name = "chrome.users.MaxConnectionsPerProxy"
    schema_values = {
      maxConnectionsPerProxy = jsonencode(33)
    }
  }
}

data "googleworkspace_chrome_resolved_policies" "test" {
  org_unit_id          = googleworkspace_chrome_policy.test.id
  policy_schema_filter = "chrome.users.MaxConnectionsPerProxy"
}
`, ouName)
}
