// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccResourceChromePolicyGroupPriorityOrdering_app(t *testing.T) {
	t.Parallel()

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")
	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	prefix := fmt.Sprintf("tf-test-%s", acctest.RandString(10))

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccResourceChromePolicyGroupPriorityOrdering_app(prefix, domainName, "first", "second"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("googleworkspace_chrome_policy_group_priority_ordering.test", "group_ids.0", "googleworkspace_group.first", "id"),
					resource.TestCheckResourceAttrPair("googleworkspace_chrome_policy_group_priority_ordering.test", "group_ids.1", "googleworkspace_group.second", "id"),
				),
			},
			{
				Config: testAccResourceChromePolicyGroupPriorityOrdering_app(prefix, domainName, "second", "first"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair("googleworkspace_chrome_policy_group_priority_ordering.test", "group_ids.0", "googleworkspace_group.second", "id"),
					resource.TestCheckResourceAttrPair("googleworkspace_chrome_policy_group_priority_ordering.test", "group_ids.1", "googleworkspace_group.first", "id"),
				),
			},
			{
				ResourceName:      "googleworkspace_chrome_policy_group_priority_ordering.test",
				ImportState:       true,
				ImportStateId:     "namespace=chrome.users.apps::app_id=" + testAccChromePolicyAppId,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceChromePolicyGroupPriorityOrdering_app(prefix, domainName, high, low string) string {
	return fmt.Sprintf(`
resource "googleworkspace_group" "first" {
  email = "%[1]s-first@%[2]s"
}

resource "googleworkspace_group" "second" {
  email = "%[1]s-second@%[2]s"
}

resource "googleworkspace_chrome_group_policy" "first" {
  group_id = googleworkspace_group.first.id
  additional_target_keys {
    target_key   = "app_id"
    target_value = "%[3]s"
  }
  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode("FORCED")
    }
  }
}

resource "googleworkspace_chrome_group_policy" "second" {
  group_id = googleworkspace_group.second.id
  additional_target_keys {
    target_key   = "app_id"
    target_value = "%[3]s"
  }
  policies {
    schema_name = "chrome.users.apps.InstallType"
    schema_values = {
      appInstallType = jsonencode("BLOCKED")
    }
  }
}

resource "googleworkspace_chrome_policy_group_priority_ordering" "test" {
  policy_namespace = "chrome.users.apps"
  policy_target_key {
    additional_target_key_names = {
      app_id = "%[3]s"
    }
  }
  group_ids = [
    googleworkspace_group.%[4]s.id,
    googleworkspace_group.%[5]s.id,
  ]

  depends_on = [
    googleworkspace_chrome_group_policy.first,
    googleworkspace_chrome_group_policy.second,
  ]
}
`, prefix, domainName, testAccChromePolicyAppId, high, low)
}
