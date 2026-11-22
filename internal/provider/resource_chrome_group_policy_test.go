// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccResourceChromeGroupPolicy_appScoped(t *testing.T) {
	t.Parallel()

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")
	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	email := fmt.Sprintf("tf-test-%s@%s", acctest.RandString(10), domainName)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccResourceChromeGroupPolicy_appScoped(email, "FORCED"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("googleworkspace_chrome_group_policy.test", "group_id", "googleworkspace_group.test", "id"),
					resource.TestCheckResourceAttr("googleworkspace_chrome_group_policy.test", "policies.0.schema_values.appInstallType", "\"FORCED\""),
				),
			},
			{
				Config: testAccResourceChromeGroupPolicy_appScoped(email, "BLOCKED"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("googleworkspace_chrome_group_policy.test", "policies.0.schema_values.appInstallType", "\"BLOCKED\""),
				),
			},
			{
				ResourceName: "googleworkspace_chrome_group_policy.test",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources["googleworkspace_chrome_group_policy.test"]
					if !ok {
						return "", fmt.Errorf("resource not found: googleworkspace_chrome_group_policy.test")
					}
					return fmt.Sprintf("%s/app_id=%s/chrome.users.apps.InstallType", rs.Primary.ID, testAccChromePolicyAppId), nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"applied_values"},
			},
		},
	})
}

func testAccResourceChromeGroupPolicy_appScoped(email, installType string) string {
	return fmt.Sprintf(`
resource "googleworkspace_group" "test" {
  email = "%s"
}

resource "googleworkspace_chrome_group_policy" "test" {
  group_id = googleworkspace_group.test.id
  additional_target_keys {
    target_key   = "app_id"
    target_value = "%s"
  }
  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode("%s")
    }
  }
}
`, email, testAccChromePolicyAppId, installType)
}
